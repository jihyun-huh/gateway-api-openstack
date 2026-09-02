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
	"github.com/jihyun-huh/gateway-api-openstack/test/e2e/internal/cloudauth"
	"github.com/jihyun-huh/gateway-api-openstack/test/e2e/internal/controllercontract"
)

const (
	backendName           = "backend"
	gatewayName           = "edge"
	routeName             = "basic"
	listenerName          = "http"
	backendPort           = 8080
	listenerPort          = 80
	standardChannel       = "standard"
	emergencyCleanupLimit = 15 * time.Minute
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
	projectIDResolver      cloudauth.ProjectIDResolver
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
		t.Skip("run the suite through make test-e2e with an explicit E2E_CONFIG")
	}

	startedAt := time.Now().UTC()
	report := newE2EReport(startedAt, config.Project.Mode, config.RestartMode, config.ControllerRevision, config.ControllerImageDigest)
	markUnimplementedFaultChecks(report)

	ctx, cancel := context.WithTimeout(context.Background(), config.Timeout)
	defer cancel()

	suite := requirePhase2Suite(t, config, report)
	runErr := runPhase2WithCleanup(ctx, suite, report)
	runErr = verifyPhase2PostCleanupAudit(suite, report, runErr)
	runErr = writePhase2Artifacts(t, config.ArtifactDirectory, report, runErr)
	if runErr != nil {
		t.Fatal("Phase 2 E2E did not complete; details were intentionally redacted from test output")
	}
}

func requirePhase2Suite(t *testing.T, config e2eConfig, report *e2eReport) *phase2Suite {
	t.Helper()
	suite, err := newPhase2Suite(config, report)
	if err == nil {
		return suite
	}
	mustSetCheck(report, "preflight safety validation", statusFailed, failedSummary("preflight safety validation"))
	writeReportAfterSetupFailure(t, config.ArtifactDirectory, report)
	t.Fatal("Phase 2 E2E preflight could not create a Kubernetes client")
	return nil
}

func runPhase2WithCleanup(ctx context.Context, suite *phase2Suite, report *e2eReport) error {
	runErr := suite.run(ctx)
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), emergencyCleanupLimit)
	cleanupErr := suite.ensureCleanup(cleanupCtx)
	cleanupCancel()
	if cleanupErr == nil {
		return runErr
	}
	mustSetCheck(report, "orderly deletion and finalizer completion", statusFailed, failedSummary("orderly deletion and finalizer completion"))
	return firstPhase2Error(runErr, errors.New("ordered E2E cleanup did not complete"))
}

func verifyPhase2PostCleanupAudit(suite *phase2Suite, report *e2eReport, runErr error) error {
	if suite.baselineAudit == nil {
		return runErr
	}
	auditCtx, auditCancel := context.WithTimeout(context.Background(), emergencyCleanupLimit)
	err := suite.verifyPostCleanupAudit(auditCtx)
	auditCancel()
	if err == nil {
		return runErr
	}
	mustSetCheck(report, "post-test ownership audit returns to baseline", statusFailed, failedSummary("post-test ownership audit returns to baseline"))
	return firstPhase2Error(runErr, errors.New("post-test ownership audit did not match the baseline"))
}

func writePhase2Artifacts(t *testing.T, directory string, report *e2eReport, runErr error) error {
	t.Helper()
	if err := writeE2EArtifacts(directory, report); err != nil {
		t.Errorf("Phase 2 E2E artifacts could not be written: %v", err)
		return firstPhase2Error(runErr, errors.New("E2E artifacts could not be written"))
	}
	return runErr
}

func firstPhase2Error(current, fallback error) error {
	if current != nil {
		return current
	}
	return fallback
}

func newPhase2Suite(config e2eConfig, report *e2eReport) (*phase2Suite, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = config.Kubeconfig
	overrides := &clientcmd.ConfigOverrides{CurrentContext: config.KubeContext}
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load explicit kubeconfig and context: %w", err)
	}
	restConfig.Timeout = config.HTTPTimeout

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
			Timeout: config.HTTPTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		gatewayClassName: config.GatewayClassName,
		hostname:         "e2e-" + config.RunID + ".example.test",
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
	if err := s.verifyPreflightFiles(); err != nil {
		return err
	}
	if err := s.verifyPreflightKubernetesState(ctx); err != nil {
		return err
	}
	return s.prepareBaselineAudit(ctx)
}

func (s *phase2Suite) verifyPreflightFiles() error {
	if _, err := os.Lstat(s.config.ArtifactDirectory); !os.IsNotExist(err) {
		if err == nil {
			return fmt.Errorf("artifact directory already exists")
		}
		return fmt.Errorf("inspect artifact directory: %w", err)
	}
	info, err := os.Stat(s.config.Kubeconfig)
	if err != nil {
		return fmt.Errorf("inspect kubeconfig: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("kubeconfig is not a regular file")
	}
	return nil
}

func (s *phase2Suite) verifyPreflightKubernetesState(ctx context.Context) error {
	if err := s.verifyExplicitContext(); err != nil {
		return err
	}
	if err := s.verifyGatewayAPIBundle(ctx); err != nil {
		return err
	}
	if err := s.requireAbsent(ctx, client.ObjectKey{Name: s.gatewayClassName}, &gatewayv1.GatewayClass{}); err != nil {
		return fmt.Errorf("verify unique GatewayClass: %w", err)
	}
	if err := s.requireAbsent(ctx, client.ObjectKey{Name: s.config.Namespace}, &corev1.Namespace{}); err != nil {
		return fmt.Errorf("verify unique Namespace: %w", err)
	}
	if err := s.verifyControllerDeployment(ctx); err != nil {
		return err
	}
	return nil
}

func (s *phase2Suite) prepareBaselineAudit(ctx context.Context) error {
	info, err := os.Stat(s.config.Audit.Binary)
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
	return nil
}

func (s *phase2Suite) verifyExplicitContext() error {
	raw, err := clientcmd.LoadFromFile(s.config.Kubeconfig)
	if err != nil {
		return fmt.Errorf("load kubeconfig metadata: %w", err)
	}
	if _, found := raw.Contexts[s.config.KubeContext]; !found {
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
	if err := s.verifyRunScopedController(ctx, deployment); err != nil {
		return err
	}
	if err := s.validateControllerRollout(deployment, pods); err != nil {
		return err
	}
	if err := s.verifyControllerPods(ctx, deployment, pods); err != nil {
		return err
	}
	return s.verifyControllerLease(ctx, pods)
}

func (s *phase2Suite) validateControllerRollout(deployment *appsv1.Deployment, pods []corev1.Pod) error {
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != s.config.ControllerReplicas {
		return fmt.Errorf("controller Deployment replica count does not match the explicit configuration")
	}
	if deployment.Generation != deployment.Status.ObservedGeneration ||
		deployment.Status.UpdatedReplicas != s.config.ControllerReplicas ||
		deployment.Status.ReadyReplicas != s.config.ControllerReplicas ||
		deployment.Status.AvailableReplicas != s.config.ControllerReplicas ||
		deployment.Status.UnavailableReplicas != 0 {
		return fmt.Errorf("controller Deployment is not fully rolled out")
	}
	if len(pods) != int(s.config.ControllerReplicas) {
		return fmt.Errorf("controller Deployment has an unexpected number of selected pods")
	}
	return nil
}

func (s *phase2Suite) verifyControllerPods(ctx context.Context, deployment *appsv1.Deployment, pods []corev1.Pod) error {
	for index := range pods {
		if err := s.verifyControllerPod(ctx, deployment, &pods[index]); err != nil {
			return err
		}
	}
	return nil
}

func (s *phase2Suite) verifyControllerLease(ctx context.Context, pods []corev1.Pod) error {
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
	if deployment.Annotations[runAnnotation] != s.config.RunID {
		return fmt.Errorf("controller Deployment lacks the exact run annotation")
	}
	container, found := findContainer(deployment.Spec.Template.Spec.Containers, s.config.ControllerContainer)
	if !found {
		return fmt.Errorf("configured controller container is absent")
	}
	if !strings.HasSuffix(container.Image, "@"+s.config.ControllerImageDigest) {
		return fmt.Errorf("controller Deployment image is not pinned to the expected digest")
	}
	if err := controllercontract.ValidateMetricsPorts(container.Ports); err != nil {
		return err
	}
	if err := controllercontract.ValidateArguments(container.Args, s.config.Audit.Microversion); err != nil {
		return err
	}
	return nil
}

func (s *phase2Suite) readyControllerPods(ctx context.Context) (*appsv1.Deployment, []corev1.Pod, error) {
	var deployment appsv1.Deployment
	key := client.ObjectKey{Namespace: s.config.ControllerNamespace, Name: s.config.ControllerDeployment}
	if err := s.client.Get(ctx, key, &deployment); err != nil {
		return nil, nil, fmt.Errorf("read controller Deployment: %w", err)
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return nil, nil, fmt.Errorf("build controller Deployment selector: %w", err)
	}
	var podList corev1.PodList
	if err := s.client.List(ctx, &podList, client.InNamespace(s.config.ControllerNamespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, nil, fmt.Errorf("list controller pods: %w", err)
	}
	return &deployment, podList.Items, nil
}

func (s *phase2Suite) verifyControllerPod(ctx context.Context, deployment *appsv1.Deployment, pod *corev1.Pod) error {
	if !podReady(pod) {
		return fmt.Errorf("controller pod is not Ready")
	}
	replicaSetOwner, err := controllerReplicaSetOwner(pod)
	if err != nil {
		return err
	}
	var replicaSet appsv1.ReplicaSet
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: replicaSetOwner.Name}, &replicaSet); err != nil {
		return fmt.Errorf("read controller ReplicaSet owner: %w", err)
	}
	if err := verifyReplicaSetOwnerUID(replicaSetOwner, &replicaSet); err != nil {
		return err
	}
	if err := verifyReplicaSetDeploymentOwner(&replicaSet, deployment); err != nil {
		return err
	}
	return s.verifyControllerPodImage(pod)
}

func controllerReplicaSetOwner(pod *corev1.Pod) (*metav1.OwnerReference, error) {
	replicaSetOwner := metav1.GetControllerOf(pod)
	if replicaSetOwner == nil || replicaSetOwner.Kind != "ReplicaSet" || replicaSetOwner.APIVersion != appsv1.SchemeGroupVersion.String() {
		return nil, fmt.Errorf("controller pod does not have the expected ReplicaSet owner")
	}
	return replicaSetOwner, nil
}

func verifyReplicaSetOwnerUID(owner *metav1.OwnerReference, replicaSet *appsv1.ReplicaSet) error {
	if owner.UID == "" || replicaSet.UID != owner.UID {
		return fmt.Errorf("controller pod ReplicaSet owner UID does not match")
	}
	return nil
}

func verifyReplicaSetDeploymentOwner(replicaSet *appsv1.ReplicaSet, deployment *appsv1.Deployment) error {
	deploymentOwner := metav1.GetControllerOf(replicaSet)
	if deploymentOwner == nil || deploymentOwner.APIVersion != appsv1.SchemeGroupVersion.String() ||
		deploymentOwner.Kind != "Deployment" || deploymentOwner.Name != deployment.Name || deploymentOwner.UID != deployment.UID {
		return fmt.Errorf("controller pod is outside the configured Deployment")
	}
	return nil
}

func (s *phase2Suite) verifyControllerPodImage(pod *corev1.Pod) error {
	status, found := findContainerStatus(pod.Status.ContainerStatuses, s.config.ControllerContainer)
	if !found || !status.Ready ||
		(status.ImageID != s.config.ControllerImageDigest && !strings.HasSuffix(status.ImageID, "@"+s.config.ControllerImageDigest)) {
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
