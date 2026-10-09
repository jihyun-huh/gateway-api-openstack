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
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestBackendProfileRestrictsPlacementAndLocalMembers(t *testing.T) {
	selector := map[string]string{"e2e.example.test/backend": "true"}
	suite := &phase2Suite{config: e2eConfig{
		Namespace:                    "test-backend",
		BackendExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal,
		BackendNodeSelector:          selector,
	}}
	deployment := suite.backendDeployment(2)
	podSpec := deployment.Spec.Template.Spec
	if !reflect.DeepEqual(podSpec.NodeSelector, selector) {
		t.Fatalf("backend Node selector = %#v", podSpec.NodeSelector)
	}
	if podSpec.Affinity == nil || podSpec.Affinity.PodAntiAffinity == nil {
		t.Fatal("backend Pods do not require distinct Nodes")
	}
	terms := podSpec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if len(terms) != 1 || terms[0].TopologyKey != corev1.LabelHostname ||
		!reflect.DeepEqual(terms[0].LabelSelector.MatchLabels, deployment.Spec.Template.Labels) || len(terms[0].Namespaces) != 0 {
		t.Fatalf("backend anti-affinity does not isolate this Namespace's replicas: %#v", terms)
	}
	service := suite.backendService()
	if service.Spec.Type != corev1.ServiceTypeNodePort || service.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		t.Fatalf("backend Service does not restrict members to local endpoint Nodes: %#v", service.Spec)
	}
	podSpec.NodeSelector["e2e.example.test/backend"] = "changed"
	if selector["e2e.example.test/backend"] != "true" {
		t.Fatal("backend Pod selector aliases the runtime configuration")
	}
}

func TestDefaultBackendProfileKeepsClusterBehavior(t *testing.T) {
	suite := &phase2Suite{config: e2eConfig{BackendExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyCluster}}
	podSpec := suite.backendDeployment(2).Spec.Template.Spec
	if len(podSpec.NodeSelector) != 0 || podSpec.Affinity != nil ||
		suite.backendService().Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyCluster {
		t.Fatal("default backend profile changed")
	}
}

func TestBackendReadinessRejectsChangedTrafficPolicy(t *testing.T) {
	suite := selectedBackendTestSuite(t)
	deployment := suite.backendDeployment(2)
	deployment.Generation = 1
	deployment.Status = appsv1.DeploymentStatus{ObservedGeneration: 1, AvailableReplicas: 2, ReadyReplicas: 2}
	service := suite.backendService()
	service.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
	service.Spec.Ports[0].NodePort = 30001
	for _, object := range []client.Object{deployment, service} {
		if err := suite.client.Create(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	ready, err := suite.backendReady(context.Background(), deployment, service, 2)
	if err == nil || ready || !strings.Contains(err.Error(), "traffic policy changed") {
		t.Fatalf("backendReady() = %t, %v; want changed traffic policy rejection", ready, err)
	}
}

func TestSelectedBackendRequiresTwoEligibleNodesBeforeCreation(t *testing.T) {
	ready := selectedBackendTestNode("worker-ready")
	unready := selectedBackendTestNode("worker-unready")
	unready.Status.Conditions[0].Status = corev1.ConditionFalse
	unschedulable := selectedBackendTestNode("worker-unschedulable")
	unschedulable.Spec.Unschedulable = true
	foreign := selectedBackendTestNode("control-plane")
	foreign.Labels = nil
	suite := selectedBackendTestSuite(t, ready, unready, unschedulable, foreign)
	err := suite.createBackend(context.Background())
	if err == nil || !strings.Contains(err.Error(), "at least two") {
		t.Fatalf("createBackend() error = %v, want selected Node preflight failure", err)
	}
	var namespaces corev1.NamespaceList
	if err := suite.client.List(context.Background(), &namespaces); err != nil {
		t.Fatal(err)
	}
	if len(namespaces.Items) != 0 || suite.createdNamespace {
		t.Fatal("backend preflight created resources before proving two selected Nodes are eligible")
	}
}

func TestSelectedBackendEndpointsRequireDistinctSelectedNodes(t *testing.T) {
	ready, unready := true, false
	tests := []struct {
		name       string
		secondNode string
		conditions discoveryv1.EndpointConditions
		extraNode  string
		wantReady  bool
	}{
		{name: "two ready endpoints", secondNode: "worker-b", conditions: discoveryv1.EndpointConditions{Ready: &ready}, wantReady: true},
		{name: "unknown readiness is eligible", secondNode: "worker-b", wantReady: true},
		{name: "same Node", secondNode: "worker-a"},
		{name: "unselected Node", secondNode: "control-plane"},
		{name: "missing Node name"},
		{name: "unready endpoint", secondNode: "worker-b", conditions: discoveryv1.EndpointConditions{Ready: &unready}},
		{name: "terminating endpoint", secondNode: "worker-b", conditions: discoveryv1.EndpointConditions{Ready: &ready, Terminating: &ready}},
		{name: "extra endpoint on selected Node", secondNode: "worker-b", extraNode: "worker-a"},
		{name: "extra endpoint on unselected Node", secondNode: "worker-b", extraNode: "control-plane"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			firstNode := "worker-a"
			second := discoveryv1.Endpoint{Conditions: test.conditions}
			if test.secondNode != "" {
				second.NodeName = &test.secondNode
			}
			slice := &discoveryv1.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{Namespace: "test-backend", Name: "backend", Labels: map[string]string{discoveryv1.LabelServiceName: backendName}},
				Endpoints:  []discoveryv1.Endpoint{{NodeName: &firstNode, Conditions: discoveryv1.EndpointConditions{Ready: &ready}}, second},
			}
			if test.extraNode != "" {
				slice.Endpoints = append(slice.Endpoints, discoveryv1.Endpoint{NodeName: &test.extraNode})
			}
			foreign := selectedBackendTestNode("control-plane")
			foreign.Labels = nil
			suite := selectedBackendTestSuite(t, selectedBackendTestNode("worker-a"), selectedBackendTestNode("worker-b"), foreign, slice)
			got, err := suite.backendEndpointsReady(context.Background(), 2)
			if err != nil || got != test.wantReady {
				t.Fatalf("backendEndpointsReady() = %t, %v; want %t", got, err, test.wantReady)
			}
		})
	}
}

func selectedBackendTestNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"e2e.example.test/backend": "true"}},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
}

func selectedBackendTestSuite(t *testing.T, objects ...client.Object) *phase2Suite {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, discoveryv1.AddToScheme} {
		if err := install(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return &phase2Suite{
		client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		config: e2eConfig{
			Namespace:                    "test-backend",
			BackendExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal,
			BackendNodeSelector:          map[string]string{"e2e.example.test/backend": "true"},
		},
	}
}
