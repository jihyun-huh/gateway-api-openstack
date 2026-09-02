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

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

type runOptions struct {
	repositoryRoot      string
	auditBinary         string
	goBinary            string
	stdout              io.Writer
	stderr              io.Writer
	authenticateProject projectAuthenticator
	runAudit            ownershipAuditRunner
}

func run(ctx context.Context, config resolvedConfig, options runOptions) error {
	if options.goBinary == "" {
		options.goBinary = "go"
	}
	if options.auditBinary != "" {
		config.auditBinary = options.auditBinary
	}
	if options.stdout == nil {
		options.stdout = io.Discard
	}
	if options.stderr == nil {
		options.stderr = io.Discard
	}
	_, _ = fmt.Fprintf(options.stdout, "Starting E2E run %s\n", config.runID)
	if err := validateLocalInputs(config); err != nil {
		return err
	}
	cloudsYAML, err := os.ReadFile(config.controllerCloudsYAML)
	if err != nil {
		return fmt.Errorf("read controller clouds.yaml: %w", err)
	}

	kubeClient, err := newKubernetesClient(config)
	if err != nil {
		return err
	}
	if err := preflightKubernetes(ctx, kubeClient, config); err != nil {
		return err
	}
	if err := preflightOpenStack(ctx, config, cloudsYAML, options.authenticateProject, options.runAudit); err != nil {
		return err
	}
	base, err := loadBaseObjects(options.repositoryRoot)
	if err != nil {
		return err
	}
	objects, err := buildControllerObjects(base, config, cloudsYAML)
	if err != nil {
		return err
	}
	installed, err := installController(ctx, kubeClient, objects, config)
	if err != nil {
		if !installed.deploymentAttempted && len(installed.objects) > 0 {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			cleanupErr := installed.remove(cleanupCtx, kubeClient)
			cancel()
			if cleanupErr != nil {
				return fmt.Errorf("install run-scoped controller; partial installation requires review: %w", errors.Join(err, cleanupErr))
			}
			return fmt.Errorf("install run-scoped controller; removed the partial installation: %w", err)
		}
		return fmt.Errorf("install run-scoped controller; retained objects require review: %w", err)
	}
	_, _ = fmt.Fprintf(options.stdout, "Prepared E2E run %s in controller namespace %s\n", config.runID, config.controllerNamespace)

	if err := waitForController(ctx, kubeClient, config, objects.deployment.UID); err != nil {
		return fmt.Errorf("wait for run-scoped controller; installation was retained: %w", err)
	}
	if err := runHarness(ctx, config, options); err != nil {
		return fmt.Errorf("run shared-project E2E; controller installation was retained: %w", err)
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := installed.remove(cleanupCtx, kubeClient); err != nil {
		return fmt.Errorf("remove successful e2e controller installation: %w", err)
	}
	_, _ = fmt.Fprintf(options.stdout, "Completed E2E run %s and removed its controller installation\n", config.runID)
	return nil
}

func validateLocalInputs(config resolvedConfig) error {
	for _, item := range []struct {
		name       string
		path       string
		executable bool
	}{
		{name: "kubeconfig", path: config.kubeconfig},
		{name: "controller clouds.yaml", path: config.controllerCloudsYAML},
		{name: "audit clouds.yaml", path: config.auditCloudsYAML},
		{name: "ownership audit binary", path: config.auditBinary, executable: true},
	} {
		info, err := os.Stat(item.path)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", item.name, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s must be a regular file", item.name)
		}
		if item.executable && info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("%s must be executable", item.name)
		}
	}
	if _, err := os.Lstat(config.artifactDirectory); !os.IsNotExist(err) {
		if err == nil {
			return fmt.Errorf("artifact directory already exists")
		}
		return fmt.Errorf("inspect artifact directory: %w", err)
	}
	return nil
}

func newKubernetesClient(config resolvedConfig) (client.Client, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = config.kubeconfig
	overrides := &clientcmd.ConfigOverrides{CurrentContext: config.kubeContext}
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load explicit kubeconfig and context: %w", err)
	}
	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		rbacv1.AddToScheme,
		coordinationv1.AddToScheme,
		gatewayv1.Install,
	} {
		if err := install(scheme); err != nil {
			return nil, fmt.Errorf("install kubernetes scheme: %w", err)
		}
	}
	kubeClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create kubernetes client: %w", err)
	}
	return kubeClient, nil
}

func preflightKubernetes(ctx context.Context, kubeClient client.Client, config resolvedConfig) error {
	for _, namespace := range []string{config.workloadNamespace, config.controllerNamespace} {
		var existing corev1.Namespace
		err := kubeClient.Get(ctx, client.ObjectKey{Name: namespace}, &existing)
		if err == nil {
			return fmt.Errorf("preflight requires the run-scoped Kubernetes namespaces to be absent")
		}
		if client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("read run-scoped Namespace during preflight: %w", err)
		}
	}

	var classes gatewayv1.GatewayClassList
	if err := kubeClient.List(ctx, &classes); err != nil {
		return fmt.Errorf("list GatewayClasses during preflight: %w", err)
	}
	for index := range classes.Items {
		class := &classes.Items[index]
		if class.Name == config.gatewayClassName || string(class.Spec.ControllerName) == config.controllerName {
			return fmt.Errorf("preflight found a GatewayClass collision for the run identity")
		}
	}
	return nil
}

func waitForController(ctx context.Context, kubeClient client.Client, config resolvedConfig, deploymentUID types.UID) error {
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	return wait.PollUntilContextCancel(waitCtx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		var deployment appsv1.Deployment
		key := client.ObjectKey{Namespace: config.controllerNamespace, Name: controllerDeploymentName}
		if err := kubeClient.Get(ctx, key, &deployment); err != nil {
			return false, nil
		}
		if deployment.UID != deploymentUID || deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != controllerReplicas ||
			deployment.Generation != deployment.Status.ObservedGeneration || deployment.Status.UpdatedReplicas != controllerReplicas ||
			deployment.Status.ReadyReplicas != controllerReplicas || deployment.Status.AvailableReplicas != controllerReplicas ||
			deployment.Status.UnavailableReplicas != 0 {
			return false, nil
		}
		selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
		if err != nil {
			return false, fmt.Errorf("build controller deployment selector: %w", err)
		}
		var pods corev1.PodList
		if err := kubeClient.List(ctx, &pods, client.InNamespace(config.controllerNamespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
			return false, nil
		}
		if len(pods.Items) != int(controllerReplicas) {
			return false, nil
		}
		readyNames := make(map[string]struct{}, len(pods.Items))
		for index := range pods.Items {
			pod := &pods.Items[index]
			if !runnerPodReady(pod) {
				return false, nil
			}
			status, found := runnerContainerStatus(pod.Status.ContainerStatuses, controllerContainerName)
			if !found || !status.Ready ||
				(status.ImageID != config.controllerImageDigest && !strings.HasSuffix(status.ImageID, "@"+config.controllerImageDigest)) {
				return false, nil
			}
			readyNames[pod.Name] = struct{}{}
		}

		var lease coordinationv1.Lease
		if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: config.controllerNamespace, Name: controllerLeaseName}, &lease); err != nil {
			return false, nil
		}
		if lease.Spec.HolderIdentity == nil || strings.TrimSpace(*lease.Spec.HolderIdentity) == "" || lease.Spec.RenewTime == nil ||
			lease.Spec.LeaseDurationSeconds == nil || *lease.Spec.LeaseDurationSeconds <= 0 ||
			!time.Now().Before(lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds)*time.Second)) {
			return false, nil
		}
		for podName := range readyNames {
			if *lease.Spec.HolderIdentity == podName || strings.HasPrefix(*lease.Spec.HolderIdentity, podName+"_") {
				return true, nil
			}
		}
		return false, nil
	})
}

func runnerPodReady(pod *corev1.Pod) bool {
	if pod == nil || pod.Status.Phase != corev1.PodRunning || !pod.DeletionTimestamp.IsZero() {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func runnerContainerStatus(statuses []corev1.ContainerStatus, name string) (corev1.ContainerStatus, bool) {
	for _, status := range statuses {
		if status.Name == name {
			return status, true
		}
	}
	return corev1.ContainerStatus{}, false
}

func runHarness(ctx context.Context, config resolvedConfig, options runOptions) error {
	command := exec.CommandContext(
		ctx,
		options.goBinary,
		"test",
		"-tags=e2e",
		"-count=1",
		"-run=^TestPhase2E2E$",
		"-timeout=90m",
		"./test/e2e",
	)
	command.Dir = options.repositoryRoot
	command.Env = mergeHarnessEnvironment(os.Environ(), config.harnessEnvironment())
	command.Stdout = options.stdout
	command.Stderr = options.stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("e2e test process failed: %w", err)
	}
	return nil
}

func mergeHarnessEnvironment(current []string, overrides map[string]string) []string {
	result := make([]string, 0, len(current)+len(overrides))
	for _, entry := range current {
		name, _, found := strings.Cut(entry, "=")
		if found && (strings.HasPrefix(name, "GATEWAY_OPENSTACK_") || strings.HasPrefix(name, "OS_")) {
			continue
		}
		result = append(result, entry)
	}
	names := make([]string, 0, len(overrides))
	for name := range overrides {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		result = append(result, name+"="+overrides[name])
	}
	return result
}
