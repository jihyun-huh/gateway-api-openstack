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
	"os"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/jihyun-huh/gateway-api-openstack/test/e2e/internal/runconfig"
)

func TestObserveControllerReadyRequiresDeploymentPodsAndCurrentLease(t *testing.T) {
	config, err := resolveFileConfig(validFileConfig(), resolveOptions{repositoryRoot: testRepositoryRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	deploymentUID := types.UID("deployment-uid")
	replicas := config.ControllerReplicas
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  config.ControllerNamespace,
			Name:       config.ControllerDeployment,
			UID:        deploymentUID,
			Generation: 3,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"run": config.RunID}},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 3,
			UpdatedReplicas:    replicas,
			ReadyReplicas:      replicas,
			AvailableReplicas:  replicas,
		},
	}
	pods := make([]*corev1.Pod, 0, replicas)
	for _, name := range []string{"controller-a", "controller-b"} {
		pods = append(pods, readyControllerPod(config, name))
	}
	holder := pods[0].Name
	duration := int32(15)
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: config.ControllerNamespace, Name: config.LeaderLease},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holder,
			RenewTime:            &metav1.MicroTime{Time: now.Add(-time.Second)},
			LeaseDurationSeconds: &duration,
		},
	}
	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, coordinationv1.AddToScheme} {
		if err := install(scheme); err != nil {
			t.Fatal(err)
		}
	}
	objects := []runtime.Object{deployment, lease}
	for _, pod := range pods {
		objects = append(objects, pod)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()

	ready, err := observeControllerReady(context.Background(), kubeClient, config, deploymentUID, now)
	if err != nil || !ready {
		t.Fatalf("observeControllerReady() = %t, %v; want true, nil", ready, err)
	}
	ready, err = observeControllerReady(context.Background(), kubeClient, config, deploymentUID, now.Add(time.Minute))
	if err != nil || ready {
		t.Fatalf("observeControllerReady(expired lease) = %t, %v; want false, nil", ready, err)
	}
}

func TestRunHarnessPassesOnlyPrivateRuntimeSelection(t *testing.T) {
	config, err := resolveFileConfig(validFileConfig(), resolveOptions{repositoryRoot: testRepositoryRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	marker := filepath.Join(directory, "runtime-copy.json")
	script := filepath.Join(directory, "fake-go")
	contents := []byte(`#!/bin/sh
test "$GATEWAY_OPENSTACK_E2E" = true || exit 10
test -f "$GATEWAY_OPENSTACK_E2E_RUNTIME_CONFIG" || exit 11
test -z "${GATEWAY_OPENSTACK_API_QPS+x}" || exit 12
test -z "${OS_AUTH_URL+x}" || exit 13
cp "$GATEWAY_OPENSTACK_E2E_RUNTIME_CONFIG" "$RUNNER_TEST_RUNTIME_COPY"
`)
	if err := os.WriteFile(script, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GATEWAY_OPENSTACK_API_QPS", "ambient-override")
	t.Setenv("OS_AUTH_URL", "https://ambient.example.test")
	t.Setenv("RUNNER_TEST_RUNTIME_COPY", marker)
	if err := runHarness(context.Background(), config, runOptions{
		repositoryRoot: testRepositoryRoot(t),
		goBinary:       script,
	}); err != nil {
		t.Fatalf("runHarness() error = %v", err)
	}
	if err := os.Chmod(marker, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := runconfig.LoadRuntime(marker)
	if err != nil {
		t.Fatalf("LoadRuntime(copied handoff) error = %v", err)
	}
	if loaded.RunID != config.RunID || loaded.Project.Mode != config.Project.Mode {
		t.Fatalf("runtime handoff = %#v", loaded)
	}
}

func readyControllerPod(config resolvedConfig, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: config.ControllerNamespace,
			Name:      name,
			Labels:    map[string]string{"run": config.RunID},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionTrue,
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: config.ControllerContainer, Ready: true, ImageID: config.ControllerImageDigest,
			}},
		},
	}
}
