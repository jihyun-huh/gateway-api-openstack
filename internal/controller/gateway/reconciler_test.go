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
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller/graph"
)

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
	client := indexedFakeClientBuilder(scheme, testConfig()).WithStatusSubresource(class, gateway).WithObjects(class, gateway).Build()
	provider := &recordingProvider{}
	reconciler := Reconciler{Client: client, Provider: provider, Coordinator: &graph.Coordinator{}, APIReader: client, Config: testConfig()}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "edge"}}
	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result != (ctrl.Result{Requeue: true}) || len(provider.gatewaySpecs) != 0 {
		t.Fatalf("binding checkpoint result = %#v, EnsureGateway calls = %d", result, len(provider.gatewaySpecs))
	}
	secondResult, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	wantResync := controller.OpenStackResyncAfter(reconciler.Config.OpenStackResyncInterval, gateway.UID)
	if secondResult.RequeueAfter != wantResync {
		t.Fatalf("second Reconcile() RequeueAfter = %s, want %s", secondResult.RequeueAfter, wantResync)
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
	resourceVersion := got.ResourceVersion
	thirdResult, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("third Reconcile() error = %v", err)
	}
	if thirdResult.RequeueAfter != wantResync || len(provider.gatewaySpecs) != 2 {
		t.Fatalf("third Reconcile() result = %#v, EnsureGateway calls = %d", thirdResult, len(provider.gatewaySpecs))
	}
	if err := client.Get(context.Background(), request.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.ResourceVersion != resourceVersion {
		t.Fatalf("converged resync changed Gateway resourceVersion from %s to %s", resourceVersion, got.ResourceVersion)
	}
}

func TestGatewayInvalidSpecCleansPreviouslyManagedResources(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-uid", Finalizers: []string{cfg.GatewayFinalizer()}, Annotations: gatewayBindingAnnotations(cfg, "443")},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "openstack",
			Listeners:        []gatewayv1.Listener{{Name: "https", Protocol: gatewayv1.HTTPSProtocolType, Port: 443}},
		},
	}
	kubeClient := indexedFakeClientBuilder(scheme, testConfig()).WithStatusSubresource(gateway).WithObjects(class, gateway).Build()
	provider := &recordingProvider{}
	reconciler := Reconciler{Client: kubeClient, Provider: provider, Coordinator: &graph.Coordinator{}, APIReader: kubeClient, Config: cfg}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}}
	if result, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	} else if result != (ctrl.Result{Requeue: true}) {
		t.Fatalf("Reconcile() result = %#v, want status checkpoint", result)
	}
	if len(provider.deletedGateways) != 0 {
		t.Fatalf("DeleteGateway calls before status checkpoint = %d, want 0", len(provider.deletedGateways))
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if len(provider.deletedGateways) != 1 || provider.deletedGateways[0].GatewayUID != string(gateway.UID) {
		t.Fatalf("deleted Gateway identities = %#v", provider.deletedGateways)
	}
	var got gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if controllerFinalizer := cfg.GatewayFinalizer(); controllerutil.ContainsFinalizer(&got, controllerFinalizer) {
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
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-uid", Finalizers: []string{cfg.GatewayFinalizer()}, Annotations: gatewayBindingAnnotations(cfg, "443")},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "openstack",
			Listeners:        []gatewayv1.Listener{{Name: "https", Protocol: gatewayv1.HTTPSProtocolType, Port: 443}},
		},
	}
	kubeClient := indexedFakeClientBuilder(scheme, testConfig()).WithStatusSubresource(gateway).WithObjects(class, gateway).Build()
	provider := &recordingProvider{gatewayDeleteErr: errors.New("temporary delete failure")}
	reconciler := Reconciler{Client: kubeClient, Provider: provider, Coordinator: &graph.Coordinator{}, APIReader: kubeClient, Config: cfg}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}}
	if result, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	} else if result != (ctrl.Result{Requeue: true}) {
		t.Fatalf("first Reconcile() result = %#v, want status checkpoint", result)
	}
	var got gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}, &got); err != nil {
		t.Fatal(err)
	}
	accepted := meta.FindStatusCondition(got.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionFalse || accepted.Reason != string(gatewayv1.GatewayReasonListenersNotValid) {
		t.Fatalf("Accepted condition = %#v", accepted)
	}
	if !controllerutil.ContainsFinalizer(&got, cfg.GatewayFinalizer()) {
		t.Fatal("cleanup failure removed the safety finalizer")
	}
	if len(provider.deletedGateways) != 0 {
		t.Fatalf("DeleteGateway calls before status checkpoint = %d, want 0", len(provider.deletedGateways))
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err == nil {
		t.Fatal("second Reconcile() unexpectedly succeeded")
	}
}

func TestGatewayClassHandoffCleansControllerResources(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "other"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: "other.example/controller"}}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-uid", Finalizers: []string{cfg.GatewayFinalizer()}, Annotations: gatewayBindingAnnotations(cfg, "80")},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "other",
			Listeners:        []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}},
		},
	}
	kubeClient := indexedFakeClientBuilder(scheme, testConfig()).WithStatusSubresource(gateway).WithObjects(class, gateway).Build()
	provider := &recordingProvider{}
	reconciler := Reconciler{Client: kubeClient, Provider: provider, Coordinator: &graph.Coordinator{}, APIReader: kubeClient, Config: cfg}
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
	if controllerutil.ContainsFinalizer(&got, cfg.GatewayFinalizer()) {
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
			Finalizers:  []string{cfg.GatewayFinalizer()},
			Annotations: gatewayBindingAnnotations(cfg, "80"),
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "openstack",
			Listeners:        []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 8080}},
		},
	}
	kubeClient := indexedFakeClientBuilder(scheme, testConfig()).WithStatusSubresource(gateway).WithObjects(class, gateway).Build()
	provider := &recordingProvider{}
	reconciler := Reconciler{Client: kubeClient, Provider: provider, Coordinator: &graph.Coordinator{}, APIReader: kubeClient, Config: cfg}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}}
	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result != (ctrl.Result{Requeue: true}) {
		t.Fatalf("Reconcile() result = %#v, want status checkpoint", result)
	}
	if len(provider.deletedGateways) != 0 || len(provider.gatewaySpecs) != 0 {
		t.Fatalf("replacement deletes = %d, ensures = %d", len(provider.deletedGateways), len(provider.gatewaySpecs))
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if len(provider.deletedGateways) != 1 || len(provider.gatewaySpecs) != 0 {
		t.Fatalf("replacement deletes = %d, ensures = %d", len(provider.deletedGateways), len(provider.gatewaySpecs))
	}
	var got gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(&got, cfg.GatewayFinalizer()) || got.Annotations[cfg.GatewayListenerPortAnnotation()] != "" {
		t.Fatalf("Gateway replacement metadata = finalizers %#v, annotations %#v", got.Finalizers, got.Annotations)
	}
	if result, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("third Reconcile() error = %v", err)
	} else if result != (ctrl.Result{Requeue: true}) {
		t.Fatalf("third Reconcile() result = %#v, want binding checkpoint", result)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("fourth Reconcile() error = %v", err)
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
			Finalizers: []string{cfg.GatewayFinalizer()},
			Annotations: map[string]string{
				cfg.GatewayListenerPortAnnotation(): "80",
				cfg.GatewayClusterIDAnnotation():    "original-cluster",
				cfg.GatewayProjectIDAnnotation():    cfg.OpenStackProjectID,
			},
		},
		Spec: gatewayv1.GatewaySpec{GatewayClassName: "openstack", Listeners: []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}}},
	}
	kubeClient := indexedFakeClientBuilder(scheme, testConfig()).WithStatusSubresource(gateway).WithObjects(class, gateway).Build()
	provider := &recordingProvider{}
	reconciler := Reconciler{Client: kubeClient, APIReader: kubeClient, Provider: provider, Config: cfg}
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
	if !controllerutil.ContainsFinalizer(&got, cfg.GatewayFinalizer()) {
		t.Fatal("identity drift removed the safety finalizer")
	}
}

func TestGatewayOpenStackFailurePreservesAttachedRouteCount(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "edge", UID: "gateway-uid", Generation: 1,
			Finalizers:  []string{cfg.GatewayFinalizer()},
			Annotations: gatewayBindingAnnotations(cfg, "80"),
		},
		Spec: gatewayv1.GatewaySpec{GatewayClassName: "openstack", Listeners: []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}}},
	}
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "api", Generation: 1},
		Spec: gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{
			Name: "edge",
		}}}},
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
	kubeClient := indexedFakeClientBuilder(scheme, testConfig()).WithStatusSubresource(gateway, route, otherRoute).WithObjects(class, gateway, route, otherRoute).Build()
	provider := &recordingProvider{gatewayErr: cloud.NewProviderError(cloud.ErrorCategoryRetryableService, errors.New("temporary Octavia failure"))}
	reconciler := Reconciler{Client: kubeClient, Provider: provider, Coordinator: &graph.Coordinator{}, APIReader: kubeClient, Config: cfg}
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
	kubeClient := indexedFakeClientBuilder(scheme, testConfig()).WithStatusSubresource(gateway).WithObjects(class, gateway).Build()
	provider := &recordingProvider{}
	reconciler := Reconciler{Client: kubeClient, APIReader: kubeClient, Provider: provider, Config: cfg}
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
	if _, err := Validate(gateway); err == nil {
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
			if _, err := Validate(&gatewayv1.Gateway{Spec: spec}); err == nil {
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
	kubeClient := indexedFakeClientBuilder(scheme, testConfig()).WithStatusSubresource(gateway).WithObjects(class, gateway).Build()
	reconciler := Reconciler{Client: kubeClient, APIReader: kubeClient, Provider: &recordingProvider{}, Config: cfg}
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

func programmedTestGateway(cfg Config) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "edge", UID: "gateway-uid", Generation: 1,
			Finalizers:  []string{cfg.GatewayFinalizer()},
			Annotations: gatewayBindingAnnotations(cfg, "80"),
		},
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
