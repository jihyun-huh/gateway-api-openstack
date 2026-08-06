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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

func routeBindingFinalizers(cfg Config, gatewayNamespace, gatewayName, gatewayUID string) []string {
	return []string{
		cfg.routeFinalizer(),
		cfg.routeBindingFinalizer(cfg.ClusterID, cfg.OpenStackProjectID, gatewayNamespace, gatewayName, gatewayUID),
	}
}

func TestHTTPRouteDeletionUsesStoredGatewayUID(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	now := metav1.Now()
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "default",
			Name:              "api",
			UID:               "route-uid",
			DeletionTimestamp: &now,
			Finalizers:        routeBindingFinalizers(cfg, "default", "old-gateway", "old-gateway-uid"),
			Annotations: map[string]string{
				cfg.routeGatewayNamespaceAnnotation(): "default",
				cfg.routeGatewayNameAnnotation():      "old-gateway",
				cfg.routeGatewayUIDAnnotation():       "old-gateway-uid",
			},
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(route).WithObjects(route).Build()
	provider := &recordingProvider{}
	reconciler := HTTPRouteReconciler{Client: client, Provider: provider, Config: cfg}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "api"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(provider.deletedRoutes) != 1 || provider.deletedRoutes[0].GatewayUID != "old-gateway-uid" {
		t.Fatalf("deleted route identities = %#v", provider.deletedRoutes)
	}
}

func TestHTTPRouteDeletionRejectsTamperedStoredGatewayIdentity(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	now := metav1.Now()
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "default",
			Name:              "api",
			UID:               "route-uid",
			DeletionTimestamp: &now,
			Finalizers:        routeBindingFinalizers(cfg, "default", "original-gateway", "original-gateway-uid"),
			Annotations: map[string]string{
				cfg.routeGatewayNamespaceAnnotation(): "default",
				cfg.routeGatewayNameAnnotation():      "different-gateway",
				cfg.routeGatewayUIDAnnotation():       "different-gateway-uid",
			},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(route).WithObjects(route).Build()
	provider := &recordingProvider{}
	reconciler := HTTPRouteReconciler{Client: kubeClient, Provider: provider, Config: cfg}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: route.Namespace, Name: route.Name}}); err == nil {
		t.Fatal("Reconcile() accepted a tampered stored Gateway identity")
	}
	if len(provider.deletedRoutes) != 0 {
		t.Fatalf("DeleteRoute calls = %d, want 0", len(provider.deletedRoutes))
	}
	var got gatewayv1.HTTPRoute
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: route.Namespace, Name: route.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(&got, cfg.routeFinalizer()) {
		t.Fatal("tampered binding removed the safety finalizer")
	}
}

func TestGatewayReconcileProgramsAddressAndConditions(t *testing.T) {
	scheme := testScheme(t)
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: testConfig().ControllerName}}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-uid"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "openstack", Listeners: []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}}},
		Status: gatewayv1.GatewayStatus{Listeners: []gatewayv1.ListenerStatus{{Name: "http", Conditions: []metav1.Condition{{
			Type: "example.net/Custom", Status: metav1.ConditionTrue, Reason: "Preserve", LastTransitionTime: metav1.Now(),
		}}}}},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(class, gateway).WithObjects(class, gateway).Build()
	provider := &recordingProvider{}
	reconciler := GatewayReconciler{Client: client, Provider: provider, Config: testConfig()}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "edge"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	var got gatewayv1.Gateway
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "edge"}, &got); err != nil {
		t.Fatal(err)
	}
	programmed := meta.FindStatusCondition(got.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	if programmed == nil || programmed.Status != metav1.ConditionTrue {
		t.Fatalf("Programmed condition = %#v", programmed)
	}
	if len(got.Status.Addresses) != 1 || got.Status.Addresses[0].Value != "192.0.2.10" {
		t.Fatalf("Gateway addresses = %#v", got.Status.Addresses)
	}
	if len(got.Status.Listeners) != 1 || meta.FindStatusCondition(got.Status.Listeners[0].Conditions, "example.net/Custom") == nil {
		t.Fatalf("custom Listener condition was not preserved: %#v", got.Status.Listeners)
	}
	if len(provider.gatewaySpecs) != 1 || provider.gatewaySpecs[0].Provider != "amphora" {
		t.Fatalf("gateway specs = %#v", provider.gatewaySpecs)
	}
}

func TestGatewayInvalidSpecCleansPreviouslyManagedResources(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-uid", Finalizers: []string{cfg.gatewayFinalizer()}, Annotations: gatewayBindingAnnotations(cfg, "443")},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "openstack",
			Listeners:        []gatewayv1.Listener{{Name: "https", Protocol: gatewayv1.HTTPSProtocolType, Port: 443}},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(gateway).WithObjects(class, gateway).Build()
	provider := &recordingProvider{}
	reconciler := GatewayReconciler{Client: kubeClient, Provider: provider, Config: cfg}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(provider.deletedGateways) != 1 || provider.deletedGateways[0].GatewayUID != string(gateway.UID) {
		t.Fatalf("deleted Gateway identities = %#v", provider.deletedGateways)
	}
	var got gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if controllerFinalizer := cfg.gatewayFinalizer(); controllerutil.ContainsFinalizer(&got, controllerFinalizer) {
		t.Fatalf("Gateway finalizers = %#v, still contain %q", got.Finalizers, controllerFinalizer)
	}
	accepted := meta.FindStatusCondition(got.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionFalse {
		t.Fatalf("Accepted condition = %#v", accepted)
	}
}

func TestGatewayInvalidSpecPublishesStatusBeforeCleanupRetry(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-uid", Finalizers: []string{cfg.gatewayFinalizer()}, Annotations: gatewayBindingAnnotations(cfg, "443")},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "openstack",
			Listeners:        []gatewayv1.Listener{{Name: "https", Protocol: gatewayv1.HTTPSProtocolType, Port: 443}},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(gateway).WithObjects(class, gateway).Build()
	provider := &recordingProvider{gatewayDeleteErr: errors.New("temporary delete failure")}
	reconciler := GatewayReconciler{Client: kubeClient, Provider: provider, Config: cfg}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}}); err == nil {
		t.Fatal("Reconcile() unexpectedly succeeded")
	}
	var got gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}, &got); err != nil {
		t.Fatal(err)
	}
	accepted := meta.FindStatusCondition(got.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionFalse || accepted.Reason != string(gatewayv1.GatewayReasonListenersNotValid) {
		t.Fatalf("Accepted condition = %#v", accepted)
	}
	if !controllerutil.ContainsFinalizer(&got, cfg.gatewayFinalizer()) {
		t.Fatal("cleanup failure removed the safety finalizer")
	}
}

func TestGatewayClassHandoffCleansControllerResources(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "other"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: "other.example/controller"}}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-uid", Finalizers: []string{cfg.gatewayFinalizer()}, Annotations: gatewayBindingAnnotations(cfg, "80")},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "other",
			Listeners:        []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(gateway).WithObjects(class, gateway).Build()
	provider := &recordingProvider{}
	reconciler := GatewayReconciler{Client: kubeClient, Provider: provider, Config: cfg}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(provider.deletedGateways) != 1 {
		t.Fatalf("DeleteGateway calls = %d, want 1", len(provider.deletedGateways))
	}
	var got gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(&got, cfg.gatewayFinalizer()) {
		t.Fatalf("Gateway finalizers = %#v", got.Finalizers)
	}
}

func TestGatewayListenerPortChangeReplacesOwnedGraph(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "edge", UID: "gateway-uid",
			Finalizers:  []string{cfg.gatewayFinalizer()},
			Annotations: gatewayBindingAnnotations(cfg, "80"),
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "openstack",
			Listeners:        []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 8080}},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(gateway).WithObjects(class, gateway).Build()
	provider := &recordingProvider{}
	reconciler := GatewayReconciler{Client: kubeClient, Provider: provider, Config: cfg}
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(provider.deletedGateways) != 1 || len(provider.gatewaySpecs) != 0 {
		t.Fatalf("replacement deletes = %d, ensures = %d", len(provider.deletedGateways), len(provider.gatewaySpecs))
	}
	var got gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(&got, cfg.gatewayFinalizer()) || got.Annotations[cfg.gatewayListenerPortAnnotation()] != "" {
		t.Fatalf("Gateway replacement metadata = finalizers %#v, annotations %#v", got.Finalizers, got.Annotations)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}}); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if len(provider.gatewaySpecs) != 1 || provider.gatewaySpecs[0].ListenerPort != 8080 {
		t.Fatalf("Gateway ensures = %#v", provider.gatewaySpecs)
	}
}

func TestGatewayBindingRejectsClusterIdentityDrift(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "edge", UID: "gateway-uid",
			Finalizers: []string{cfg.gatewayFinalizer()},
			Annotations: map[string]string{
				cfg.gatewayListenerPortAnnotation(): "80",
				cfg.gatewayClusterIDAnnotation():    "original-cluster",
				cfg.gatewayProjectIDAnnotation():    cfg.OpenStackProjectID,
			},
		},
		Spec: gatewayv1.GatewaySpec{GatewayClassName: "openstack", Listeners: []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}}},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(gateway).WithObjects(class, gateway).Build()
	provider := &recordingProvider{}
	reconciler := GatewayReconciler{Client: kubeClient, Provider: provider, Config: cfg}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(provider.gatewaySpecs) != 0 || len(provider.deletedGateways) != 0 {
		t.Fatalf("provider calls after identity drift: ensures %d, deletes %d", len(provider.gatewaySpecs), len(provider.deletedGateways))
	}
	var got gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}, &got); err != nil {
		t.Fatal(err)
	}
	accepted := meta.FindStatusCondition(got.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionFalse || accepted.Reason != string(gatewayv1.GatewayReasonInvalidParameters) {
		t.Fatalf("Accepted condition = %#v", accepted)
	}
	if !controllerutil.ContainsFinalizer(&got, cfg.gatewayFinalizer()) {
		t.Fatal("identity drift removed the safety finalizer")
	}
}

func TestGatewayOpenStackFailurePreservesAttachedRouteCount(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-uid", Generation: 1},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "openstack", Listeners: []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}}},
	}
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "api", Generation: 1},
		Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{{
			ParentRef: gatewayv1.ParentReference{Name: "edge"}, ControllerName: cfg.ControllerName,
			Conditions: []metav1.Condition{{
				Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue,
				ObservedGeneration: 1, Reason: string(gatewayv1.RouteReasonAccepted), LastTransitionTime: metav1.Now(),
			}},
		}}}},
	}
	wrongPort := gatewayv1.PortNumber(81)
	otherRoute := route.DeepCopy()
	otherRoute.Name = "wrong-port"
	otherRoute.Status.Parents[0].ParentRef.Port = &wrongPort
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(gateway, route, otherRoute).WithObjects(class, gateway, route, otherRoute).Build()
	provider := &recordingProvider{gatewayErr: errors.New("temporary Octavia failure")}
	reconciler := GatewayReconciler{Client: kubeClient, Provider: provider, Config: cfg}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}}); err == nil {
		t.Fatal("Reconcile() unexpectedly succeeded")
	}
	var got gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Listeners) != 1 || got.Status.Listeners[0].AttachedRoutes != 1 {
		t.Fatalf("Listener status = %#v", got.Status.Listeners)
	}
}

func TestGatewayUnsupportedAddressUsesGatewayLevelReason(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
	addressType := gatewayv1.IPAddressType
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-uid"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "openstack",
			Addresses:        []gatewayv1.GatewaySpecAddress{{Type: &addressType, Value: "192.0.2.20"}},
			Listeners:        []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(gateway).WithObjects(class, gateway).Build()
	provider := &recordingProvider{}
	reconciler := GatewayReconciler{Client: kubeClient, Provider: provider, Config: cfg}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	var got gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}, &got); err != nil {
		t.Fatal(err)
	}
	accepted := meta.FindStatusCondition(got.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionFalse || accepted.Reason != string(gatewayv1.GatewayReasonUnsupportedAddress) {
		t.Fatalf("Gateway Accepted condition = %#v", accepted)
	}
	if len(got.Status.Listeners) != 1 {
		t.Fatalf("Listener status = %#v", got.Status.Listeners)
	}
	listenerAccepted := meta.FindStatusCondition(got.Status.Listeners[0].Conditions, string(gatewayv1.ListenerConditionAccepted))
	if listenerAccepted == nil || listenerAccepted.Status != metav1.ConditionTrue {
		t.Fatalf("Listener Accepted condition = %#v", listenerAccepted)
	}
	if len(provider.gatewaySpecs) != 0 {
		t.Fatalf("EnsureGateway calls = %d, want 0", len(provider.gatewaySpecs))
	}
}

func TestRoutesForGatewayIncludesCrossNamespaceReferences(t *testing.T) {
	scheme := testScheme(t)
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "gateway-system", Name: "edge"}}
	parentNamespace := gatewayv1.Namespace(gateway.Namespace)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "application", Name: "api"},
		Spec: gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{
			Name: gatewayv1.ObjectName(gateway.Name), Namespace: &parentNamespace,
		}}}},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gateway, route).Build()
	reconciler := HTTPRouteReconciler{Client: kubeClient, Config: testConfig()}
	requests := reconciler.enqueueHTTPRoutesForGateway(context.Background(), gateway)
	if len(requests) != 1 || requests[0].NamespacedName != (types.NamespacedName{Namespace: route.Namespace, Name: route.Name}) {
		t.Fatalf("enqueueHTTPRoutesForGateway() = %#v", requests)
	}
}

func TestValidateGatewayRejectsUnsupportedAllowedRouteKinds(t *testing.T) {
	group := gatewayv1.Group(gatewayv1.GroupName)
	gateway := &gatewayv1.Gateway{Spec: gatewayv1.GatewaySpec{Listeners: []gatewayv1.Listener{{
		Name:     "http",
		Protocol: gatewayv1.HTTPProtocolType,
		Port:     80,
		AllowedRoutes: &gatewayv1.AllowedRoutes{Kinds: []gatewayv1.RouteGroupKind{{
			Group: &group,
			Kind:  "GRPCRoute",
		}}},
	}}}}
	if _, err := validateGateway(gateway); err == nil {
		t.Fatal("validateGateway() accepted unsupported GRPCRoute kind")
	}
}

func TestValidateGatewayRejectsUnsupportedGatewayWideFields(t *testing.T) {
	base := gatewayv1.GatewaySpec{Listeners: []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}}}
	tests := []struct {
		name   string
		mutate func(*gatewayv1.GatewaySpec)
	}{
		{name: "infrastructure", mutate: func(spec *gatewayv1.GatewaySpec) { spec.Infrastructure = &gatewayv1.GatewayInfrastructure{} }},
		{name: "allowed listeners", mutate: func(spec *gatewayv1.GatewaySpec) { spec.AllowedListeners = &gatewayv1.AllowedListeners{} }},
		{name: "gateway TLS", mutate: func(spec *gatewayv1.GatewaySpec) { spec.TLS = &gatewayv1.GatewayTLSConfig{} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := *base.DeepCopy()
			test.mutate(&spec)
			if _, err := validateGateway(&gatewayv1.Gateway{Spec: spec}); err == nil {
				t.Fatal("validateGateway() unexpectedly accepted unsupported field")
			}
		})
	}
}

func TestGatewayInvalidAllowedRouteKindsSetsResolvedRefs(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	group := gatewayv1.Group(gatewayv1.GroupName)
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-uid"},
		Spec: gatewayv1.GatewaySpec{GatewayClassName: "openstack", Listeners: []gatewayv1.Listener{{
			Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80,
			AllowedRoutes: &gatewayv1.AllowedRoutes{Kinds: []gatewayv1.RouteGroupKind{{Group: &group, Kind: "GRPCRoute"}}},
		}}},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(gateway).WithObjects(class, gateway).Build()
	reconciler := GatewayReconciler{Client: kubeClient, Provider: &recordingProvider{}, Config: cfg}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	var got gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Listeners) != 1 || len(got.Status.Listeners[0].SupportedKinds) != 0 {
		t.Fatalf("Listener status = %#v", got.Status.Listeners)
	}
	resolved := meta.FindStatusCondition(got.Status.Listeners[0].Conditions, string(gatewayv1.ListenerConditionResolvedRefs))
	if resolved == nil || resolved.Status != metav1.ConditionFalse || resolved.Reason != string(gatewayv1.ListenerReasonInvalidRouteKinds) {
		t.Fatalf("ResolvedRefs condition = %#v", resolved)
	}
}

func TestHTTPRouteReconcileBuildsNodePortMembers(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-uid", Finalizers: []string{cfg.gatewayFinalizer()}, Generation: 1},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "openstack", Listeners: []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}}},
		Status: gatewayv1.GatewayStatus{Conditions: []metav1.Condition{{
			Type:               string(gatewayv1.GatewayConditionProgrammed),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: 1,
			Reason:             string(gatewayv1.GatewayReasonProgrammed),
			LastTransitionTime: metav1.Now(),
		}}},
	}
	pathType := gatewayv1.PathMatchPathPrefix
	pathValue := "/api"
	backendPort := gatewayv1.PortNumber(8080)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "api", UID: "route-uid", CreationTimestamp: metav1.Now()},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "edge"}}},
			Rules:           []gatewayv1.HTTPRouteRule{{Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: &pathType, Value: &pathValue}}}, BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "backend", Port: &backendPort}}}}}},
		},
	}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "backend"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Ports: []corev1.ServicePort{{Port: 8080, NodePort: 30080}}}}
	ready := true
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "backend-1", Labels: map[string]string{discoveryv1.LabelServiceName: "backend"}}, Endpoints: []discoveryv1.Endpoint{{Conditions: discoveryv1.EndpointConditions{Ready: &ready}}}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.11"}}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(route).WithObjects(class, gateway, route, service, slice, node).Build()
	provider := &recordingProvider{}
	reconciler := HTTPRouteReconciler{Client: client, Provider: provider, Config: cfg}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "api"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(provider.routeSpecs) != 1 {
		var current gatewayv1.HTTPRoute
		_ = client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "api"}, &current)
		t.Fatalf("route specs count = %d, status = %#v", len(provider.routeSpecs), current.Status)
	}
	got := provider.routeSpecs[0]
	if got.PathType != cloud.PathMatchPrefix || got.PathValue != "/api" || len(got.Members) != 1 || got.Members[0] != (cloud.Member{Address: "10.0.0.11", Port: 30080}) {
		t.Fatalf("route spec = %#v", got)
	}
	var reconciled gatewayv1.HTTPRoute
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "api"}, &reconciled); err != nil {
		t.Fatal(err)
	}
	if reconciled.Annotations[cfg.routeGatewayUIDAnnotation()] != "gateway-uid" {
		t.Fatalf("stored Gateway UID = %q", reconciled.Annotations[cfg.routeGatewayUIDAnnotation()])
	}
}

func TestHTTPRouteDetachDeletesStoredResourcesAndMetadata(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "default",
			Name:       "api",
			UID:        "route-uid",
			Finalizers: routeBindingFinalizers(cfg, "default", "old-gateway", "old-gateway-uid"),
			Annotations: map[string]string{
				cfg.routeGatewayNamespaceAnnotation(): "default",
				cfg.routeGatewayNameAnnotation():      "old-gateway",
				cfg.routeGatewayUIDAnnotation():       "old-gateway-uid",
			},
		},
		Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{{
			ParentRef:      gatewayv1.ParentReference{Name: "old-gateway"},
			ControllerName: cfg.ControllerName,
			Conditions: []metav1.Condition{{
				Type:               string(gatewayv1.RouteConditionAccepted),
				Status:             metav1.ConditionTrue,
				Reason:             string(gatewayv1.RouteReasonAccepted),
				LastTransitionTime: metav1.Now(),
			}},
		}}}},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(route).WithObjects(route).Build()
	provider := &recordingProvider{}
	reconciler := HTTPRouteReconciler{Client: kubeClient, Provider: provider, Config: cfg}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: route.Namespace, Name: route.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(provider.deletedRoutes) != 1 || provider.deletedRoutes[0].GatewayUID != "old-gateway-uid" {
		t.Fatalf("deleted route identities = %#v", provider.deletedRoutes)
	}
	var got gatewayv1.HTTPRoute
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: route.Namespace, Name: route.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Finalizers) != 0 || got.Annotations[cfg.routeGatewayUIDAnnotation()] != "" {
		t.Fatalf("detached route metadata = finalizers %#v, annotations %#v", got.Finalizers, got.Annotations)
	}
	if len(got.Status.Parents) != 0 {
		t.Fatalf("detached route status parents = %#v", got.Status.Parents)
	}
}

func TestHTTPRouteParentChangeDetachesOldGatewayWhileNewGatewayPending(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
	newGateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "new-gateway", UID: "new-gateway-uid", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "openstack",
			Listeners:        []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}},
		},
	}
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "api", UID: "route-uid",
			Finalizers: routeBindingFinalizers(cfg, "default", "old-gateway", "old-gateway-uid"),
			Annotations: map[string]string{
				cfg.routeGatewayNamespaceAnnotation(): "default",
				cfg.routeGatewayNameAnnotation():      "old-gateway",
				cfg.routeGatewayUIDAnnotation():       "old-gateway-uid",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "new-gateway"}}},
			Rules:           []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "backend", Port: portNumberPointer(80)}}}}}},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(route).WithObjects(class, newGateway, route).Build()
	provider := &recordingProvider{}
	reconciler := HTTPRouteReconciler{Client: kubeClient, Provider: provider, Config: cfg}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: route.Namespace, Name: route.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(provider.deletedRoutes) != 1 || provider.deletedRoutes[0].GatewayUID != "old-gateway-uid" {
		t.Fatalf("deleted route identities = %#v", provider.deletedRoutes)
	}
	var got gatewayv1.HTTPRoute
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: route.Namespace, Name: route.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(&got, cfg.routeFinalizer()) || got.Annotations[cfg.routeGatewayUIDAnnotation()] != "" {
		t.Fatalf("pending route retained old binding: finalizers %#v, annotations %#v", got.Finalizers, got.Annotations)
	}
}

func TestHTTPRouteMissingBackendSetsResolvedRefsWhileGatewayPending(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
	gateway := programmedTestGateway(cfg)
	gateway.Status = gatewayv1.GatewayStatus{}
	backendPort := gatewayv1.PortNumber(8080)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "api", UID: "route-uid"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "edge"}}},
			Rules: []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{Name: "missing", Port: &backendPort},
			}}}}},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(route).WithObjects(class, gateway, route).Build()
	provider := &recordingProvider{}
	reconciler := HTTPRouteReconciler{Client: kubeClient, Provider: provider, Config: cfg}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: route.Namespace, Name: route.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	var got gatewayv1.HTTPRoute
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: route.Namespace, Name: route.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Parents) != 1 {
		t.Fatalf("status parents = %#v", got.Status.Parents)
	}
	resolved := meta.FindStatusCondition(got.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
	if resolved == nil || resolved.Status != metav1.ConditionFalse || resolved.Reason != string(gatewayv1.RouteReasonBackendNotFound) {
		t.Fatalf("ResolvedRefs condition = %#v", resolved)
	}
	if len(provider.routeSpecs) != 0 {
		t.Fatalf("EnsureRoute calls = %d, want 0", len(provider.routeSpecs))
	}
}

func TestRouteSelectionIgnoresOlderRouteForWrongListenerSection(t *testing.T) {
	scheme := testScheme(t)
	older := metav1.NewTime(time.Unix(1, 0))
	newer := metav1.NewTime(time.Unix(2, 0))
	wrongSection := gatewayv1.SectionName("wrong")
	oldRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "old", CreationTimestamp: older},
		Spec: gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{
			Name: "edge", SectionName: &wrongSection,
		}}}, Rules: []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "backend", Port: portNumberPointer(80)}}}}}}},
	}
	current := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "current", CreationTimestamp: newer},
		Spec:       gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "edge"}}}, Rules: []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "backend", Port: portNumberPointer(80)}}}}}}},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(oldRoute, current).Build()
	gateway := programmedTestGateway(testConfig())
	reconciler := HTTPRouteReconciler{Client: kubeClient, Config: testConfig()}
	selected, err := reconciler.isSelectedRoute(context.Background(), current, managedRouteParent{
		ref: current.Spec.ParentRefs[0], gateway: gateway, listener: &gateway.Spec.Listeners[0],
	})
	if err != nil {
		t.Fatalf("isSelectedRoute() error = %v", err)
	}
	if !selected {
		t.Fatal("valid route lost selection to an older route targeting a nonexistent listener section")
	}
}

func TestRouteSelectionIgnoresOlderRejectedClusterIPProfile(t *testing.T) {
	scheme := testScheme(t)
	older := metav1.NewTime(time.Unix(1, 0))
	newer := metav1.NewTime(time.Unix(2, 0))
	oldRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "old", CreationTimestamp: older},
		Spec:       gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "edge"}}}, Rules: []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "cluster-ip", Port: portNumberPointer(80)}}}}}}},
	}
	current := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "current", CreationTimestamp: newer},
		Spec:       gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "edge"}}}, Rules: []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "node-port", Port: portNumberPointer(80)}}}}}}},
	}
	clusterIP := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "cluster-ip"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP}}
	nodePort := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "node-port"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(oldRoute, current, clusterIP, nodePort).Build()
	gateway := programmedTestGateway(testConfig())
	reconciler := HTTPRouteReconciler{Client: kubeClient, Config: testConfig()}
	selected, err := reconciler.isSelectedRoute(context.Background(), current, managedRouteParent{
		ref: current.Spec.ParentRefs[0], gateway: gateway, listener: &gateway.Spec.Listeners[0],
	})
	if err != nil {
		t.Fatalf("isSelectedRoute() error = %v", err)
	}
	if !selected {
		t.Fatal("valid NodePort route lost selection to an older rejected ClusterIP profile")
	}
}

func TestRouteCanReserveSlotRejectsTerminalServiceProfiles(t *testing.T) {
	port := gatewayv1.PortNumber(80)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "candidate"},
		Spec:       gatewayv1.HTTPRouteSpec{Rules: []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "backend", Port: &port}}}}}}},
	}
	unsupportedAppProtocol := "example.com/unsupported"
	tests := []struct {
		name string
		spec corev1.ServiceSpec
	}{
		{name: "ClusterIP", spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP}},
		{name: "UDP", spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Ports: []corev1.ServicePort{{Port: 80, NodePort: 30080, Protocol: corev1.ProtocolUDP}}}},
		{name: "unsupported appProtocol", spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Ports: []corev1.ServicePort{{Port: 80, NodePort: 30080, AppProtocol: &unsupportedAppProtocol}}}},
		{name: "missing NodePort", spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Ports: []corev1.ServicePort{{Port: 80}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "backend"}, Spec: test.spec}
			kubeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(service).Build()
			reconciler := HTTPRouteReconciler{Client: kubeClient, Config: testConfig()}
			canReserve, err := reconciler.routeCanReserveSlot(context.Background(), route)
			if err != nil {
				t.Fatalf("routeCanReserveSlot() error = %v", err)
			}
			if canReserve {
				t.Fatal("terminally unsupported Service profile reserved the sole Gateway route slot")
			}
		})
	}
}

func TestTerminalServiceProfileDoesNotSelfWinOrInflateAttachedRoutes(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	class := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	gateway := programmedTestGateway(cfg)
	older := metav1.NewTime(time.Unix(1, 0))
	newer := metav1.NewTime(time.Unix(2, 0))
	terminalRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "terminal", UID: "terminal-route-uid", Generation: 1, CreationTimestamp: older},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "edge"}}},
			Rules: []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
				Name: "udp-backend", Port: portNumberPointer(80),
			}}}}}},
		},
	}
	validRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "valid", UID: "valid-route-uid", Generation: 1, CreationTimestamp: newer},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "edge"}}},
			Rules: []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
				Name: "tcp-backend", Port: portNumberPointer(80),
			}}}}}},
		},
	}
	udpService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "udp-backend"},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Ports: []corev1.ServicePort{{
			Port: 80, NodePort: 30080, Protocol: corev1.ProtocolUDP,
		}}},
	}
	tcpService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "tcp-backend"},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Ports: []corev1.ServicePort{{
			Port: 80, NodePort: 30081, Protocol: corev1.ProtocolTCP,
		}}},
	}
	ready := true
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "tcp-backend-1", Labels: map[string]string{discoveryv1.LabelServiceName: "tcp-backend"}},
		Endpoints:  []discoveryv1.Endpoint{{Conditions: discoveryv1.EndpointConditions{Ready: &ready}}},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Status: corev1.NodeStatus{
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.11"}},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).
		WithObjects(class, gateway, terminalRoute, validRoute, udpService, tcpService, slice, node).
		Build()
	provider := &recordingProvider{}
	reconciler := HTTPRouteReconciler{Client: kubeClient, Provider: provider, Config: cfg}
	ctx := context.Background()
	for _, route := range []*gatewayv1.HTTPRoute{terminalRoute, validRoute} {
		if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(route)}); err != nil {
			t.Fatalf("Reconcile(%s) error = %v", route.Name, err)
		}
	}

	var gotTerminal gatewayv1.HTTPRoute
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(terminalRoute), &gotTerminal); err != nil {
		t.Fatal(err)
	}
	if len(gotTerminal.Status.Parents) != 1 {
		t.Fatalf("terminal route parent statuses = %#v", gotTerminal.Status.Parents)
	}
	accepted := meta.FindStatusCondition(gotTerminal.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionFalse {
		t.Fatalf("terminal route Accepted condition = %#v", accepted)
	}
	resolved := meta.FindStatusCondition(gotTerminal.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
	if resolved == nil || resolved.Status != metav1.ConditionFalse || resolved.Reason != string(gatewayv1.RouteReasonUnsupportedProtocol) {
		t.Fatalf("terminal route ResolvedRefs condition = %#v", resolved)
	}

	var gotValid gatewayv1.HTTPRoute
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(validRoute), &gotValid); err != nil {
		t.Fatal(err)
	}
	if len(gotValid.Status.Parents) != 1 {
		t.Fatalf("valid route parent statuses = %#v", gotValid.Status.Parents)
	}
	accepted = meta.FindStatusCondition(gotValid.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionTrue {
		t.Fatalf("valid route Accepted condition = %#v", accepted)
	}
	programmed := meta.FindStatusCondition(gotValid.Status.Parents[0].Conditions, cfg.domain()+"/Programmed")
	if programmed == nil || programmed.Status != metav1.ConditionTrue {
		t.Fatalf("valid route Programmed condition = %#v", programmed)
	}
	if len(provider.routeSpecs) != 1 || provider.routeSpecs[0].Identity.RouteName != validRoute.Name {
		t.Fatalf("EnsureRoute specs = %#v", provider.routeSpecs)
	}

	gatewayReconciler := GatewayReconciler{Client: kubeClient, Config: cfg}
	attachedRoutes, err := gatewayReconciler.attachedRouteCount(ctx, gateway, &gateway.Spec.Listeners[0])
	if err != nil {
		t.Fatalf("attachedRouteCount() error = %v", err)
	}
	if attachedRoutes != 1 {
		t.Fatalf("attached routes = %d, want 1", attachedRoutes)
	}
}

func TestBindRouteRefetchesBeforePatchingMetadata(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Namespace:  "default",
		Name:       "api",
		UID:        "route-uid",
		Generation: 1,
	}}
	gateway := programmedTestGateway(cfg)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(route).Build()

	staleRoute := route.DeepCopy()
	current := &gatewayv1.HTTPRoute{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	current.Annotations = map[string]string{"example.com/concurrent": "preserve"}
	if err := kubeClient.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}

	reconciler := HTTPRouteReconciler{Client: kubeClient, Provider: &recordingProvider{}, Config: cfg}
	if err := reconciler.bindRoute(context.Background(), staleRoute, gateway); err != nil {
		t.Fatalf("bindRoute() error = %v", err)
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	if current.Annotations["example.com/concurrent"] != "preserve" {
		t.Fatalf("concurrent annotation was lost: %#v", current.Annotations)
	}
	if current.Annotations[cfg.routeGatewayUIDAnnotation()] != string(gateway.UID) ||
		!controllerutil.ContainsFinalizer(current, cfg.routeFinalizer()) {
		t.Fatalf("HTTPRoute binding was not patched: annotations %#v, finalizers %#v", current.Annotations, current.Finalizers)
	}
}

func TestBindRouteRejectsStaleGeneration(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	current := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Namespace:  "default",
		Name:       "api",
		UID:        "route-uid",
		Generation: 2,
	}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(current).Build()
	staleRoute := current.DeepCopy()
	staleRoute.Generation = 1

	reconciler := HTTPRouteReconciler{Client: kubeClient, Provider: &recordingProvider{}, Config: cfg}
	if err := reconciler.bindRoute(context.Background(), staleRoute, programmedTestGateway(cfg)); !errors.Is(err, errHTTPRouteChanged) {
		t.Fatalf("bindRoute() error = %v, want %v", err, errHTTPRouteChanged)
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(current), current); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(current, cfg.routeFinalizer()) {
		t.Fatal("stale reconciliation added the HTTPRoute finalizer")
	}
}

func TestDetachRouteRejectsStaleGeneration(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Namespace:  "default",
		Name:       "api",
		UID:        "route-uid",
		Generation: 2,
		Finalizers: routeBindingFinalizers(cfg, "default", "edge", "gateway-uid"),
		Annotations: map[string]string{
			cfg.routeGatewayNamespaceAnnotation(): "default",
			cfg.routeGatewayNameAnnotation():      "edge",
			cfg.routeGatewayUIDAnnotation():       "gateway-uid",
		},
	}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(route).Build()
	staleRoute := route.DeepCopy()
	staleRoute.Generation = 1
	provider := &recordingProvider{}
	reconciler := HTTPRouteReconciler{Client: kubeClient, Provider: provider, Config: cfg}

	if err := reconciler.detachRoute(context.Background(), staleRoute); !errors.Is(err, errHTTPRouteChanged) {
		t.Fatalf("detachRoute() error = %v, want %v", err, errHTTPRouteChanged)
	}
	if len(provider.deletedRoutes) != 0 {
		t.Fatalf("DeleteRoute calls = %d, want 0", len(provider.deletedRoutes))
	}
	current := &gatewayv1.HTTPRoute{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(current, cfg.routeFinalizer()) {
		t.Fatal("stale reconciliation removed the HTTPRoute finalizer")
	}
}

func TestSetRouteParentStatusesRefetchesCurrentStatus(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Namespace:  "default",
		Name:       "api",
		UID:        "route-uid",
		Generation: 1,
	}}
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).
		WithObjects(route).
		Build()
	staleRoute := route.DeepCopy()

	current := &gatewayv1.HTTPRoute{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	foreignController := gatewayv1.GatewayController("example.net/other-controller")
	current.Status.Parents = []gatewayv1.RouteParentStatus{{
		ParentRef:      gatewayv1.ParentReference{Name: "other-gateway"},
		ControllerName: foreignController,
	}}
	if err := kubeClient.Status().Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}

	reconciler := HTTPRouteReconciler{Client: kubeClient, Config: cfg}
	update := parentStatusUpdate{
		parent: gatewayv1.ParentReference{Name: "edge"},
		status: programmedRouteStatus(),
	}
	if err := reconciler.setRouteParentStatuses(context.Background(), staleRoute, []parentStatusUpdate{update}); err != nil {
		t.Fatalf("setRouteParentStatuses() error = %v", err)
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	if len(current.Status.Parents) != 2 {
		t.Fatalf("HTTPRoute parent statuses = %#v, want both controllers", current.Status.Parents)
	}
	if current.Status.Parents[0].ControllerName != foreignController || current.Status.Parents[1].ControllerName != cfg.ControllerName {
		t.Fatalf("HTTPRoute parent status controllers = %#v", current.Status.Parents)
	}
}

func portNumberPointer(value gatewayv1.PortNumber) *gatewayv1.PortNumber { return &value }

func TestNodePortMembersHonorsExternalTrafficPolicyLocal(t *testing.T) {
	scheme := testScheme(t)
	ready, notReady := true, false
	workerOne, workerTwo := "worker-1", "worker-2"
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "backend-1", Labels: map[string]string{discoveryv1.LabelServiceName: "backend"}},
		Endpoints: []discoveryv1.Endpoint{
			{NodeName: &workerOne, Conditions: discoveryv1.EndpointConditions{Ready: &ready}},
			{NodeName: &workerTwo, Conditions: discoveryv1.EndpointConditions{Ready: &notReady}},
		},
	}
	nodes := []client.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: workerOne}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.11"}}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: workerTwo}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.12"}}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
	}
	objects := append([]client.Object{slice}, nodes...)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	reconciler := HTTPRouteReconciler{Client: kubeClient, Config: testConfig()}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "backend"}, Spec: corev1.ServiceSpec{ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal}}
	members, err := reconciler.nodePortMembers(context.Background(), service, 30080)
	if err != nil {
		t.Fatalf("nodePortMembers() error = %v", err)
	}
	want := []cloud.Member{{Address: "10.0.0.11", Port: 30080}}
	if len(members) != len(want) || members[0] != want[0] {
		t.Fatalf("members = %#v, want %#v", members, want)
	}
}

func programmedTestGateway(cfg Config) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-uid", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "openstack",
			Listeners:        []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}},
		},
		Status: gatewayv1.GatewayStatus{Conditions: []metav1.Condition{{
			Type:               string(gatewayv1.GatewayConditionProgrammed),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: 1,
			Reason:             string(gatewayv1.GatewayReasonProgrammed),
			LastTransitionTime: metav1.Now(),
		}}},
	}
}
