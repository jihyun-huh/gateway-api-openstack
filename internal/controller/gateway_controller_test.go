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
	"reflect"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
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
				config.routeGatewayNamespaceAnnotation(): "previous",
				config.routeGatewayNameAnnotation():      "bound",
				config.routeGatewayUIDAnnotation():       "bound-uid",
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

	reconciler := &GatewayReconciler{Config: config}
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
	reconciler := &GatewayReconciler{Client: kubeClient}

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
			Finalizers:  []string{config.gatewayFinalizer()},
			Annotations: gatewayBindingAnnotations(config, "80"),
		},
	}
	kubeClient := indexedFakeClientBuilder(testScheme(t), config).
		WithObjects(gateway).
		Build()
	provider := &recordingProvider{}
	reconciler := &GatewayReconciler{Client: kubeClient, Provider: provider, Config: config}

	err := reconciler.cleanupGateway(context.Background(), client.ObjectKeyFromObject(gateway), "stale-resource-version")
	if !apierrors.IsConflict(err) {
		t.Fatalf("cleanupGateway() error = %v, want conflict", err)
	}
	if len(provider.deletedGateways) != 0 {
		t.Fatalf("DeleteGateway calls = %d, want 0", len(provider.deletedGateways))
	}
}
