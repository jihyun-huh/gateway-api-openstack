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

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

func TestProviderProgressRequeueAfter(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{name: "default", want: defaultProviderProgressRequeue},
		{name: "negative", interval: -time.Second, want: defaultProviderProgressRequeue},
		{name: "minimum", interval: time.Millisecond, want: minimumProviderProgressRequeue},
		{name: "provider interval", interval: 4 * time.Second, want: 4 * time.Second},
		{name: "maximum", interval: time.Hour, want: maximumProviderProgressRequeue},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outcome := cloud.ProgressingOutcome("progressing", test.interval)
			if got := providerProgressRequeueAfter(outcome); got != test.want {
				t.Fatalf("providerProgressRequeueAfter() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestProviderProgressMessage(t *testing.T) {
	if got := providerProgressMessage(cloud.ProgressingOutcome("  Creating listener  ", time.Second), "fallback"); got != "Creating listener" {
		t.Fatalf("providerProgressMessage() = %q, want Creating listener", got)
	}
	if got := providerProgressMessage(cloud.ProgressingOutcome(" ", time.Second), "fallback"); got != "fallback" {
		t.Fatalf("providerProgressMessage() = %q, want fallback", got)
	}
}

func TestGatewayProgressingOutcomeUsesBoundedRequeue(t *testing.T) {
	cfg := testConfig()
	scheme := testScheme(t)
	class := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	gateway := programmedTestGateway(cfg)
	kubeClient := indexedFakeClientBuilder(scheme, cfg).
		WithStatusSubresource(gateway).
		WithObjects(class, gateway).
		Build()
	providerResult := cloud.GatewayProgressingResult("Octavia load balancer is PENDING_UPDATE", time.Hour)
	provider := &recordingProvider{gatewayResult: &providerResult}
	reconciler := GatewayReconciler{
		Client:      kubeClient,
		APIReader:   kubeClient,
		Provider:    provider,
		Coordinator: &GraphCoordinator{},
		Config:      cfg,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != maximumProviderProgressRequeue {
		t.Fatalf("Reconcile() RequeueAfter = %s, want %s", result.RequeueAfter, maximumProviderProgressRequeue)
	}
	if len(provider.gatewaySpecs) != 1 {
		t.Fatalf("EnsureGateway calls = %d, want 1", len(provider.gatewaySpecs))
	}
	current := &gatewayv1.Gateway{}
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}, current); err != nil {
		t.Fatal(err)
	}
	programmed := meta.FindStatusCondition(current.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	if programmed == nil || programmed.Status != metav1.ConditionFalse ||
		programmed.Reason != string(gatewayv1.GatewayReasonNoResources) ||
		programmed.Message != "Octavia load balancer is PENDING_UPDATE" {
		t.Fatalf("Programmed condition = %#v", programmed)
	}
	firstResourceVersion := current.ResourceVersion
	firstTransitionTime := programmed.LastTransitionTime

	secondResult, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}})
	if err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if secondResult.RequeueAfter != maximumProviderProgressRequeue {
		t.Fatalf("second Reconcile() RequeueAfter = %s, want %s", secondResult.RequeueAfter, maximumProviderProgressRequeue)
	}
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}, current); err != nil {
		t.Fatal(err)
	}
	programmed = meta.FindStatusCondition(current.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	if programmed == nil || !programmed.LastTransitionTime.Equal(&firstTransitionTime) {
		t.Fatalf("Programmed LastTransitionTime = %#v, want %s", programmed, firstTransitionTime)
	}
	if current.ResourceVersion != firstResourceVersion {
		t.Fatalf("Gateway resource version changed from %q to %q after a semantic no-op", firstResourceVersion, current.ResourceVersion)
	}
}

func TestGatewayProviderErrorTakesPrecedenceOverProgress(t *testing.T) {
	cfg := testConfig()
	scheme := testScheme(t)
	class := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	gateway := programmedTestGateway(cfg)
	kubeClient := indexedFakeClientBuilder(scheme, cfg).
		WithStatusSubresource(gateway).
		WithObjects(class, gateway).
		Build()
	providerResult := cloud.GatewayProgressingResult("this progress must be ignored", time.Second)
	providerErr := cloud.NewProviderError(cloud.ErrorCategoryRetryableService, errors.New("Octavia is unavailable"))
	provider := &recordingProvider{gatewayResult: &providerResult, gatewayErr: providerErr}
	reconciler := GatewayReconciler{
		Client:      kubeClient,
		APIReader:   kubeClient,
		Provider:    provider,
		Coordinator: &GraphCoordinator{},
		Config:      cfg,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}})
	if !errors.Is(err, cloud.ErrRetryableService) {
		t.Fatalf("Reconcile() error = %v, want retryable service error", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("Reconcile() result = %#v, want no explicit requeue when an error is returned", result)
	}
	current := &gatewayv1.Gateway{}
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}, current); err != nil {
		t.Fatal(err)
	}
	programmed := meta.FindStatusCondition(current.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	if programmed == nil || programmed.Message == "this progress must be ignored" {
		t.Fatalf("Programmed condition = %#v", programmed)
	}
}

func TestHTTPRouteProgressingOutcomeUsesBoundedRequeue(t *testing.T) {
	cfg := testConfig()
	scheme := testScheme(t)
	class := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	gateway := programmedTestGateway(cfg)
	pathType := gatewayv1.PathMatchPathPrefix
	pathValue := "/api"
	backendPort := gatewayv1.PortNumber(8080)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "api", UID: "route-uid", Generation: 1},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "edge"}}},
			Rules: []gatewayv1.HTTPRouteRule{{
				Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: &pathType, Value: &pathValue}}},
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
					Name: "backend",
					Port: &backendPort,
				}}}},
			}},
		},
	}
	(&HTTPRouteReconciler{Config: cfg}).applyRouteBinding(route, gateway)
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "backend"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Ports: []corev1.ServicePort{{Port: 8080, NodePort: 30080}}},
	}
	ready := true
	endpointSlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "backend-1", Labels: map[string]string{discoveryv1.LabelServiceName: "backend"}},
		Endpoints:  []discoveryv1.Endpoint{{Conditions: discoveryv1.EndpointConditions{Ready: &ready}}},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Status: corev1.NodeStatus{
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.11"}},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	kubeClient := indexedFakeClientBuilder(scheme, cfg).
		WithStatusSubresource(gateway, route).
		WithObjects(class, gateway, route, service, endpointSlice, node).
		Build()
	providerResult := cloud.RouteProgressingResult("Octavia pool is PENDING_CREATE", time.Millisecond)
	provider := &recordingProvider{routeResult: &providerResult}
	reconciler := HTTPRouteReconciler{
		Client:      kubeClient,
		APIReader:   kubeClient,
		Provider:    provider,
		Coordinator: &GraphCoordinator{},
		Config:      cfg,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: route.Namespace, Name: route.Name}})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != minimumProviderProgressRequeue {
		t.Fatalf("Reconcile() RequeueAfter = %s, want %s", result.RequeueAfter, minimumProviderProgressRequeue)
	}
	if len(provider.routeSpecs) != 1 {
		t.Fatalf("EnsureRoute calls = %d, want 1", len(provider.routeSpecs))
	}
	current := &gatewayv1.HTTPRoute{}
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: route.Namespace, Name: route.Name}, current); err != nil {
		t.Fatal(err)
	}
	if len(current.Status.Parents) != 1 {
		t.Fatalf("HTTPRoute parent statuses = %#v", current.Status.Parents)
	}
	programmed := meta.FindStatusCondition(current.Status.Parents[0].Conditions, cfg.domain()+"/Programmed")
	if programmed == nil || programmed.Status != metav1.ConditionFalse || programmed.Reason != "Pending" ||
		programmed.Message != "Octavia pool is PENDING_CREATE" {
		t.Fatalf("Programmed condition = %#v", programmed)
	}
}
