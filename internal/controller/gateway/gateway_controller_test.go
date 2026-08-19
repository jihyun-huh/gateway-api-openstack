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

package gateway

import (
	"context"
	"errors"
	"reflect"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller/graph"
)

func TestGatewayRequestsForHTTPRoute(t *testing.T) {
	config := testConfig()
	gatewayNamespace := gatewayv1.Namespace("gateway-system")
	otherGroup := gatewayv1.Group("example.com")
	otherKind := gatewayv1.Kind("Other")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "api",
			Annotations: map[string]string{
				config.RouteGatewayNamespaceAnnotation(): "previous",
				config.RouteGatewayNameAnnotation():      "bound",
				config.RouteGatewayUIDAnnotation():       "bound-uid",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{
			{Name: "edge"},
			{Name: "edge"},
			{Name: "shared", Namespace: &gatewayNamespace},
			{Name: "other-group", Group: &otherGroup},
			{Name: "other-kind", Kind: &otherKind},
		}}},
		Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{
			{ParentRef: gatewayv1.ParentReference{Name: "status-parent"}, ControllerName: config.ControllerName},
			{ParentRef: gatewayv1.ParentReference{Name: "foreign-status"}, ControllerName: "example.com/foreign"},
		}}},
	}

	reconciler := &Reconciler{Config: config}
	requests := reconciler.gatewayRequestsForHTTPRoute(context.Background(), route)
	want := []reconcile.Request{
		{NamespacedName: types.NamespacedName{Namespace: "default", Name: "edge"}},
		{NamespacedName: types.NamespacedName{Namespace: "default", Name: "status-parent"}},
		{NamespacedName: types.NamespacedName{Namespace: "gateway-system", Name: "shared"}},
		{NamespacedName: types.NamespacedName{Namespace: "previous", Name: "bound"}},
	}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("gatewayRequestsForHTTPRoute() = %#v, want %#v", requests, want)
	}
	if requests := reconciler.gatewayRequestsForHTTPRoute(context.Background(), &gatewayv1.Gateway{}); requests != nil {
		t.Fatalf("requests for non-HTTPRoute object = %#v, want nil", requests)
	}
}

func TestGatewayRequestsForGatewayClass(t *testing.T) {
	matchingGateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "openstack"},
	}
	otherGateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "other"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "other"},
	}
	kubeClient := indexedFakeClientBuilder(testScheme(t), testConfig()).
		WithObjects(matchingGateway, otherGateway).
		Build()
	reconciler := &Reconciler{Client: kubeClient}

	requests := reconciler.gatewayRequestsForGatewayClass(context.Background(), &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack"},
	})
	want := []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(matchingGateway)}}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("gatewayRequestsForGatewayClass() = %#v, want %#v", requests, want)
	}
	if requests := reconciler.gatewayRequestsForGatewayClass(context.Background(), matchingGateway); requests != nil {
		t.Fatalf("requests for non-GatewayClass object = %#v, want nil", requests)
	}
}

func TestCleanupGatewayRejectsStaleResourceVersion(t *testing.T) {
	config := testConfig()
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "default",
			Name:        "edge",
			UID:         "gateway-uid",
			Finalizers:  []string{config.GatewayFinalizer()},
			Annotations: gatewayBindingAnnotations(config, "80"),
		},
	}
	kubeClient := indexedFakeClientBuilder(testScheme(t), config).
		WithObjects(gateway).
		Build()
	provider := &recordingProvider{}
	reconciler := &Reconciler{Client: kubeClient, Provider: provider, Config: config}

	expected := gateway.DeepCopy()
	expected.ResourceVersion = "stale-resource-version"
	scope := &gatewayScope{gateway: expected, original: expected.DeepCopy()}
	_, err := reconciler.cleanupGateway(context.Background(), scope)
	if !apierrors.IsConflict(err) {
		t.Fatalf("cleanupGateway() error = %v, want conflict", err)
	}
	if len(provider.deletedGateways) != 0 {
		t.Fatalf("DeleteGateway calls = %d, want 0", len(provider.deletedGateways))
	}
}

func TestGatewayBindingWithoutFinalizerRemainsCleanupResponsibility(t *testing.T) {
	config := testConfig()
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "example.net/other-controller"},
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "default",
			Name:        "edge",
			UID:         "gateway-uid",
			Annotations: gatewayBindingAnnotations(config, "80"),
		},
		Spec: gatewayv1.GatewaySpec{GatewayClassName: "openstack"},
	}
	kubeClient := indexedFakeClientBuilder(testScheme(t), config).
		WithObjects(gatewayClass, gateway).
		Build()
	provider := &recordingProvider{}
	reconciler := &Reconciler{
		Client: kubeClient, APIReader: kubeClient, Provider: provider,
		Coordinator: &graph.Coordinator{}, Config: config,
	}

	if _, err := reconciler.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(gateway),
	}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(provider.deletedGateways) != 1 {
		t.Fatalf("DeleteGateway calls = %d, want 1", len(provider.deletedGateways))
	}
	var current gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), &current); err != nil {
		t.Fatal(err)
	}
	if controller.GatewayHasControllerBinding(config, &current) {
		t.Fatalf("Gateway binding remained after cleanup: finalizers %#v, annotations %#v", current.Finalizers, current.Annotations)
	}
}

func TestGatewayScopeRefreshRejectsConcurrentStatusChange(t *testing.T) {
	config := testConfig()
	original := programmedTestGateway(config)
	original.ResourceVersion = "1"
	desired := original.DeepCopy()
	SetCondition(&desired.Status.Conditions, Condition(
		string(gatewayv1.GatewayConditionAccepted),
		metav1.ConditionFalse,
		string(gatewayv1.GatewayReasonInvalid),
		"Gateway configuration changed",
		desired.Generation,
	))
	live := original.DeepCopy()
	live.ResourceVersion = "2"
	live.Status.Listeners = []gatewayv1.ListenerStatus{{
		Name: "http",
		Conditions: []metav1.Condition{Condition(
			"example.net/Observed",
			metav1.ConditionTrue,
			"Observed",
			"Preserve this condition",
			live.Generation,
		)},
	}}
	scope := &gatewayScope{gateway: desired, original: original}

	if err := scope.refresh(live); !errors.Is(err, errGatewayChanged) {
		t.Fatalf("refresh() error = %v, want %v", err, errGatewayChanged)
	}
	if !reflect.DeepEqual(scope.gateway.Status, desired.Status) || scope.gateway.ResourceVersion != "1" {
		t.Fatalf("refresh() changed scope after conflict: %#v", scope.gateway)
	}
}
