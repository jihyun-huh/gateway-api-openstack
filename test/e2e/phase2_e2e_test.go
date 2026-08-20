//go:build e2e

/*
Copyright 2026 The gateway-api-openstack Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayconsts "sigs.k8s.io/gateway-api/pkg/consts"

	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
)

const (
	gatewayClassNamePrefix = "gao-e2e-"
	backendName            = "backend"
	gatewayName            = "edge"
	routeName              = "basic"
	listenerName           = "http"
	backendPort            = 8080
	listenerPort           = 80
	controllerMetricsPort  = 8080
	standardChannel        = "standard"
	emergencyCleanupLimit  = 15 * time.Minute
)

var requiredBundleCRDs = []string{
	"gatewayclasses.gateway.networking.k8s.io",
	"gateways.gateway.networking.k8s.io",
	"httproutes.gateway.networking.k8s.io",
}

type phase2Suite struct {
	config         e2eConfig
	client         client.Client
	coreRESTClient rest.Interface
	report         *e2eReport

	httpClient             *http.Client
	gatewayClassName       string
	hostname               string
	createdNamespace       bool
	createdClass           bool
	createdGateway         bool
	createdRoute           bool
	controllerScaled       bool
	baselineAudit          *auditSummary
	activeAudit            *auditSummary
	activeAuditFingerprint [32]byte

	controllerDeploymentUID types.UID
	namespaceUID            types.UID
	gatewayClassUID         types.UID
	gatewayUID              types.UID
	routeUID                types.UID
}

type leaseState struct {
	holder      string
	transitions int32
}

func TestPhase2E2E(t *testing.T) {
	config, enabled, err := loadE2EConfig(os.Getenv)
	if err != nil {
		t.Fatalf("Phase 2 E2E configuration was rejected: %v", err)
	}
	if !enabled {
		t.Skip("set GATEWAY_OPENSTACK_E2E=true and the dedicated-project acknowledgement to run this suite")
	}

	startedAt := time.Now().UTC()
	report := newE2EReport(startedAt, config.restartMode, config.controllerRevision, config.controllerImageDigest)
	markUnimplementedFaultChecks(report)

	ctx, cancel := context.WithTimeout(context.Background(), config.timeout)
	defer cancel()

	suite, err := newPhase2Suite(config, report)
	if err != nil {
		mustSetCheck(report, "preflight safety validation", statusFailed, failedSummary("preflight safety validation"))
		writeReportAfterSetupFailure(t, config.artifactDirectory, report)
		t.Fatal("Phase 2 E2E preflight could not create a Kubernetes client")
	}

	runErr := suite.run(ctx)
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), emergencyCleanupLimit)
	cleanupErr := suite.ensureCleanup(cleanupCtx)
	cleanupCancel()
	if cleanupErr != nil {
		mustSetCheck(report, "orderly deletion and finalizer completion", statusFailed, failedSummary("orderly deletion and finalizer completion"))
		if runErr == nil {
			runErr = errors.New("ordered E2E cleanup did not complete")
		}
	}

	if config.audit.enabled && suite.baselineAudit != nil {
		auditCtx, auditCancel := context.WithTimeout(context.Background(), emergencyCleanupLimit)
		if err := suite.verifyPostCleanupAudit(auditCtx); err != nil {
			mustSetCheck(report, "post-test ownership audit returns to baseline", statusFailed, failedSummary("post-test ownership audit returns to baseline"))
			if runErr == nil {
				runErr = errors.New("post-test ownership audit did not match the baseline")
			}
		}
		auditCancel()
	}

	if err := writeE2EArtifacts(config.artifactDirectory, report); err != nil {
		t.Errorf("Phase 2 E2E artifacts could not be written: %v", err)
		if runErr == nil {
			runErr = errors.New("E2E artifacts could not be written")
		}
	}
	if runErr != nil {
		t.Fatal("Phase 2 E2E did not complete; details were intentionally redacted from test output")
	}
}

func newPhase2Suite(config e2eConfig, report *e2eReport) (*phase2Suite, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = config.kubeconfig
	overrides := &clientcmd.ConfigOverrides{CurrentContext: config.kubeContext}
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load explicit kubeconfig and context: %w", err)
	}
	restConfig.Timeout = config.httpTimeout

	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		coordinationv1.AddToScheme,
		discoveryv1.AddToScheme,
		apiextensionsv1.AddToScheme,
		gatewayv1.Install,
	} {
		if err := install(scheme); err != nil {
			return nil, fmt.Errorf("install Kubernetes scheme: %w", err)
		}
	}
	kubeClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes pod proxy client: %w", err)
	}

	return &phase2Suite{
		config:         config,
		client:         kubeClient,
		coreRESTClient: clientset.CoreV1().RESTClient(),
		report:         report,
		httpClient: &http.Client{
			Timeout: config.httpTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		gatewayClassName: gatewayClassNamePrefix + config.runID,
		hostname:         "e2e-" + config.runID + ".example.test",
	}, nil
}

func (s *phase2Suite) run(ctx context.Context) error {
	steps := []struct {
		name string
		run  func(context.Context) error
	}{
		{name: "preflight safety validation", run: s.preflight},
		{name: "isolated NodePort backend", run: s.createBackend},
		{name: "GatewayClass status", run: s.createGatewayClass},
		{name: "Gateway status", run: s.createGateway},
		{name: "HTTPRoute status", run: s.createHTTPRoute},
		{name: "active ownership inventory audit", run: s.verifyActiveOwnershipInventoryAudit},
		{name: "external HTTP traffic", run: s.verifyExternalTraffic},
		{name: "leader pod deletion and recovery", run: s.deleteLeaderAndVerifyRecovery},
		{name: "cold controller restart and recovery", run: s.coldRestartAndVerifyRecovery},
		{name: "converged metrics snapshot", run: s.verifyConvergedNoOp},
		{name: "orderly deletion and finalizer completion", run: s.orderlyCleanup},
	}
	incomplete := false
	for _, step := range steps {
		if err := step.run(ctx); err != nil {
			if errors.Is(err, errEvidenceNotConfigured) {
				mustSetCheck(s.report, step.name, statusNotRun, checkSummaryEvidenceNotConfigured)
				incomplete = true
				continue
			}
			mustSetCheck(s.report, step.name, statusFailed, failedSummary(step.name))
			return errors.New("an E2E step failed")
		}
		mustSetCheck(s.report, step.name, statusPassed, passedSummary(step.name))
	}
	if incomplete {
		return errEvidenceNotConfigured
	}
	return nil
}

func (s *phase2Suite) preflight(ctx context.Context) error {
	if _, err := os.Lstat(s.config.artifactDirectory); !os.IsNotExist(err) {
		if err == nil {
			return fmt.Errorf("artifact directory already exists")
		}
		return fmt.Errorf("inspect artifact directory: %w", err)
	}
	info, err := os.Stat(s.config.kubeconfig)
	if err != nil {
		return fmt.Errorf("inspect kubeconfig: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("kubeconfig is not a regular file")
	}
	if err := s.verifyExplicitContext(); err != nil {
		return err
	}
	if err := s.verifyGatewayAPIBundle(ctx); err != nil {
		return err
	}
	if err := s.requireAbsent(ctx, client.ObjectKey{Name: s.gatewayClassName}, &gatewayv1.GatewayClass{}); err != nil {
		return fmt.Errorf("verify unique GatewayClass: %w", err)
	}
	if err := s.requireAbsent(ctx, client.ObjectKey{Name: s.config.namespace}, &corev1.Namespace{}); err != nil {
		return fmt.Errorf("verify unique Namespace: %w", err)
	}
	if err := s.verifyControllerDeployment(ctx); err != nil {
		return err
	}
	if s.config.audit.enabled {
		info, err := os.Stat(s.config.audit.binary)
		if err != nil {
			return fmt.Errorf("inspect audit binary: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("audit binary is not an executable regular file")
		}
		summary, err := s.runOwnershipAudit(ctx, true, nil)
		if err != nil {
			return err
		}
		s.baselineAudit = &summary
		s.report.Audit = &auditEvidence{Baseline: &summary}
	} else {
		mustSetCheck(s.report, "post-test ownership audit returns to baseline", statusNotRun, checkSummaryAuditNotConfigured)
	}
	return nil
}

func (s *phase2Suite) verifyExplicitContext() error {
	raw, err := clientcmd.LoadFromFile(s.config.kubeconfig)
	if err != nil {
		return fmt.Errorf("load kubeconfig metadata: %w", err)
	}
	if _, found := raw.Contexts[s.config.kubeContext]; !found {
		return fmt.Errorf("configured kubeconfig context is absent")
	}
	return nil
}

func (s *phase2Suite) verifyGatewayAPIBundle(ctx context.Context) error {
	observation, err := controller.ObserveGatewayAPIVersion(ctx, s.client)
	if err != nil {
		return fmt.Errorf("observe Gateway API bundle: %w", err)
	}
	if !observation.Supported {
		return fmt.Errorf("Gateway API bundle version is unsupported")
	}
	var definitions apiextensionsv1.CustomResourceDefinitionList
	if err := s.client.List(ctx, &definitions); err != nil {
		return fmt.Errorf("list installed Gateway API CRDs: %w", err)
	}
	present := make(map[string]struct{})
	for _, definition := range definitions.Items {
		if !controller.IsGatewayAPICRDName(definition.Name) {
			continue
		}
		present[definition.Name] = struct{}{}
		if definition.Annotations[gatewayconsts.BundleVersionAnnotation] != gatewayconsts.BundleVersion ||
			definition.Annotations[gatewayconsts.ChannelAnnotation] != standardChannel {
			return fmt.Errorf("an installed Gateway API CRD is not from the pinned Standard bundle")
		}
	}
	for _, name := range requiredBundleCRDs {
		if _, found := present[name]; !found {
			return fmt.Errorf("a required Gateway API CRD is absent")
		}
	}
	return nil
}

func (s *phase2Suite) verifyControllerDeployment(ctx context.Context) error {
	deployment, pods, err := s.readyControllerPods(ctx)
	if err != nil {
		return err
	}
	if err := s.validateControllerDeploymentIdentity(deployment); err != nil {
		return err
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != s.config.controllerReplicas {
		return fmt.Errorf("controller Deployment replica count does not match the explicit configuration")
	}
	if deployment.Generation != deployment.Status.ObservedGeneration ||
		deployment.Status.UpdatedReplicas != s.config.controllerReplicas ||
		deployment.Status.ReadyReplicas != s.config.controllerReplicas ||
		deployment.Status.AvailableReplicas != s.config.controllerReplicas ||
		deployment.Status.UnavailableReplicas != 0 {
		return fmt.Errorf("controller Deployment is not fully rolled out")
	}
	if len(pods) != int(s.config.controllerReplicas) {
		return fmt.Errorf("controller Deployment has an unexpected number of selected pods")
	}
	for index := range pods {
		if err := s.verifyControllerPod(ctx, deployment, &pods[index]); err != nil {
			return err
		}
	}
	lease, err := s.currentLease(ctx)
	if err != nil {
		return err
	}
	if _, err := matchLeaseHolder(lease.holder, pods); err != nil {
		return err
	}
	return nil
}

func (s *phase2Suite) validateControllerDeploymentIdentity(deployment *appsv1.Deployment) error {
	if deployment == nil || deployment.UID == "" {
		return fmt.Errorf("controller Deployment has no immutable UID")
	}
	if s.controllerDeploymentUID == "" {
		s.controllerDeploymentUID = deployment.UID
	} else if deployment.UID != s.controllerDeploymentUID {
		return fmt.Errorf("controller Deployment UID changed during the run")
	}
	if deployment.Annotations[dedicatedRunAnnotation] != s.config.runID {
		return fmt.Errorf("controller Deployment lacks the exact dedicated-run annotation")
	}
	container, found := findContainer(deployment.Spec.Template.Spec.Containers, s.config.controllerContainer)
	if !found {
		return fmt.Errorf("configured controller container is absent")
	}
	if !strings.HasSuffix(container.Image, "@"+s.config.controllerImageDigest) {
		return fmt.Errorf("controller Deployment image is not pinned to the expected digest")
	}
	metricsPorts := 0
	for _, port := range container.Ports {
		if port.Name != "metrics" {
			continue
		}
		metricsPorts++
		if port.ContainerPort != controllerMetricsPort || port.Protocol != corev1.ProtocolTCP {
			return fmt.Errorf("controller Deployment has an unexpected metrics container port")
		}
	}
	if metricsPorts != 1 {
		return fmt.Errorf("controller Deployment must expose one named metrics container port")
	}
	leaderElectionArguments := 0
	metricsAddressArguments := 0
	for _, argument := range container.Args {
		if strings.HasPrefix(argument, "--leader-elect") {
			leaderElectionArguments++
			if argument != "--leader-elect=true" {
				return fmt.Errorf("controller Deployment has an unsafe leader election argument")
			}
		}
		if strings.HasPrefix(argument, "--metrics-bind-address") {
			metricsAddressArguments++
			if argument != "--metrics-bind-address=:8080" {
				return fmt.Errorf("controller Deployment has an unsafe metrics bind address")
			}
		}
	}
	if leaderElectionArguments != 1 {
		return fmt.Errorf("controller Deployment must enable leader election exactly once")
	}
	if metricsAddressArguments != 1 {
		return fmt.Errorf("controller Deployment must bind the validated metrics port exactly once")
	}
	return nil
}

func (s *phase2Suite) readyControllerPods(ctx context.Context) (*appsv1.Deployment, []corev1.Pod, error) {
	var deployment appsv1.Deployment
	key := client.ObjectKey{Namespace: s.config.controllerNamespace, Name: s.config.controllerDeployment}
	if err := s.client.Get(ctx, key, &deployment); err != nil {
		return nil, nil, fmt.Errorf("read controller Deployment: %w", err)
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return nil, nil, fmt.Errorf("build controller Deployment selector: %w", err)
	}
	var podList corev1.PodList
	if err := s.client.List(ctx, &podList, client.InNamespace(s.config.controllerNamespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, nil, fmt.Errorf("list controller pods: %w", err)
	}
	return &deployment, podList.Items, nil
}

func (s *phase2Suite) verifyControllerPod(ctx context.Context, deployment *appsv1.Deployment, pod *corev1.Pod) error {
	if !podReady(pod) {
		return fmt.Errorf("controller pod is not Ready")
	}
	replicaSetOwner := metav1.GetControllerOf(pod)
	if replicaSetOwner == nil || replicaSetOwner.Kind != "ReplicaSet" || replicaSetOwner.APIVersion != appsv1.SchemeGroupVersion.String() {
		return fmt.Errorf("controller pod does not have the expected ReplicaSet owner")
	}
	var replicaSet appsv1.ReplicaSet
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: replicaSetOwner.Name}, &replicaSet); err != nil {
		return fmt.Errorf("read controller ReplicaSet owner: %w", err)
	}
	if replicaSetOwner.UID == "" || replicaSet.UID != replicaSetOwner.UID {
		return fmt.Errorf("controller pod ReplicaSet owner UID does not match")
	}
	deploymentOwner := metav1.GetControllerOf(&replicaSet)
	if deploymentOwner == nil || deploymentOwner.APIVersion != appsv1.SchemeGroupVersion.String() ||
		deploymentOwner.Kind != "Deployment" || deploymentOwner.Name != deployment.Name || deploymentOwner.UID != deployment.UID {
		return fmt.Errorf("controller pod is outside the configured Deployment")
	}
	status, found := findContainerStatus(pod.Status.ContainerStatuses, s.config.controllerContainer)
	if !found || !status.Ready ||
		(status.ImageID != s.config.controllerImageDigest && !strings.HasSuffix(status.ImageID, "@"+s.config.controllerImageDigest)) {
		return fmt.Errorf("controller pod is not running the expected immutable image")
	}
	return nil
}

func (s *phase2Suite) requireAbsent(ctx context.Context, key client.ObjectKey, object client.Object) error {
	err := s.client.Get(ctx, key, object)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("object already exists")
}

func markUnimplementedFaultChecks(report *e2eReport) {
	for _, name := range []string{
		"external deletion of an owned child resource",
		"blocked finalization",
		"quota failure",
		"request timeout and rate limiting",
		"Octavia resource failure",
	} {
		mustSetCheck(report, name, statusNotRun, checkSummaryFaultNotConfigured)
	}
}

func mustSetCheck(report *e2eReport, name string, status checkStatus, summary string) {
	if err := report.setCheck(name, status, summary); err != nil {
		panic(err)
	}
}

func passedSummary(string) string {
	return checkSummaryPassed
}

func failedSummary(string) string {
	return checkSummaryFailed
}

func writeReportAfterSetupFailure(t *testing.T, directory string, report *e2eReport) {
	t.Helper()
	if err := writeE2EArtifacts(directory, report); err != nil {
		t.Error("Phase 2 E2E failure artifacts could not be written")
	}
}
