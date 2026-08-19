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

package httproute

import (
	"context"
	"errors"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayconsts "sigs.k8s.io/gateway-api/pkg/consts"

	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
	gatewaycontroller "github.com/jihyun-huh/gateway-api-openstack/internal/controller/gateway"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller/graph"
)

func TestHTTPRouteMutationBoundaryChecksLiveGatewayAPIVersion(t *testing.T) {
	cfg := testConfig()
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	gateway := programmedTestGateway(cfg)
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-uid"
	route.Generation = 1
	(&Reconciler{Config: cfg}).applyRouteBinding(route, gateway)
	service, endpointSlice, node := validNodePortBackend("default", "backend")
	cachedClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithObjects(gatewayClass, gateway, route, service, endpointSlice, node).
		Build()
	liveReader := liveReaderWithUnsupportedVersion(cachedClient)
	provider := &recordingProvider{}
	reconciler := &Reconciler{
		Client:      cachedClient,
		APIReader:   liveReader,
		Provider:    provider,
		Coordinator: &graph.Coordinator{},
		Config:      cfg,
	}

	_, err := reconciler.ensureRouteGraph(context.Background(), route.DeepCopy(), gateway.DeepCopy())
	if !errors.Is(err, controller.ErrUnsupportedGatewayAPIVersion) {
		t.Fatalf("ensureRouteGraph() error = %v, want unsupported Gateway API version", err)
	}
	if len(provider.routeSpecs) != 0 {
		t.Fatalf("EnsureRoute calls = %d, want 0", len(provider.routeSpecs))
	}
}

func TestHTTPRouteReconcileChecksLiveVersionBeforeDetach(t *testing.T) {
	cfg := testConfig()
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	gateway := programmedTestGateway(cfg)
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-uid"
	route.Generation = 1
	(&Reconciler{Config: cfg}).applyRouteBinding(route, gateway)
	route.Spec.ParentRefs = append(route.Spec.ParentRefs, gatewayv1.ParentReference{Name: "edge"})
	cachedClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(route).
		WithObjects(gatewayClass, gateway, route).
		Build()
	liveReader := liveReaderWithUnsupportedVersion(cachedClient)
	provider := &recordingProvider{}
	reconciler := &Reconciler{
		Client: cachedClient, APIReader: liveReader, Provider: provider,
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}

	request := ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(route),
	}
	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result != (ctrl.Result{Requeue: true}) {
		t.Fatalf("first Reconcile() result = %#v, want status checkpoint", result)
	}
	if len(provider.deletedRoutes) != 0 || len(provider.routeSpecs) != 0 {
		t.Fatalf("provider calls before status checkpoint = deletes %d, ensures %d; want none", len(provider.deletedRoutes), len(provider.routeSpecs))
	}

	result, err = reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("live Gateway API version mismatch did not schedule a fallback recheck")
	}
	if len(provider.deletedRoutes) != 0 || len(provider.routeSpecs) != 0 {
		t.Fatalf("provider calls = deletes %d, ensures %d; want none", len(provider.deletedRoutes), len(provider.routeSpecs))
	}
	var got gatewayv1.HTTPRoute
	if err := cachedClient.Get(context.Background(), client.ObjectKeyFromObject(route), &got); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(&got, cfg.RouteFinalizer()) {
		t.Fatal("HTTPRoute detach removed the stored binding during a CRD version mismatch")
	}
}

func TestUnsupportedGatewayAPIVersionFreezesBoundGraph(t *testing.T) {
	cfg := testConfig()
	gatewayClass := unsupportedVersionGatewayClass(cfg)
	gateway := programmedTestGateway(cfg)
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-uid"
	route.Generation = 1
	routeBinder := &Reconciler{Config: cfg}
	routeBinder.applyRouteBinding(route, gateway)
	route.Status.Parents = []gatewayv1.RouteParentStatus{{
		ParentRef:      route.Spec.ParentRefs[0],
		ControllerName: cfg.ControllerName,
		Conditions: []metav1.Condition{
			Condition(string(gatewayv1.RouteConditionAccepted), metav1.ConditionTrue, string(gatewayv1.RouteReasonAccepted), "HTTPRoute is accepted", route.Generation),
			Condition(string(gatewayv1.RouteConditionResolvedRefs), metav1.ConditionTrue, string(gatewayv1.RouteReasonResolvedRefs), "References are resolved", route.Generation),
			Condition(cfg.Domain()+"/Programmed", metav1.ConditionTrue, "Programmed", "HTTPRoute is programmed", route.Generation),
		},
	}}
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(gateway, route).
		WithObjects(gatewayClass, gateway, route).
		Build()
	provider := &recordingProvider{}
	gatewayReconciler := &gatewaycontroller.Reconciler{
		Client: kubeClient, APIReader: kubeClient, Provider: provider,
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}
	routeReconciler := &Reconciler{
		Client: kubeClient, APIReader: kubeClient, Provider: provider,
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}

	_, err := gatewayReconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gateway),
	})
	if err != nil {
		t.Fatalf("Gateway Reconcile() error = %v", err)
	}
	_, err = routeReconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(route),
	})
	if err != nil {
		t.Fatalf("HTTPRoute Reconcile() error = %v", err)
	}
	if len(provider.gatewaySpecs) != 0 || len(provider.routeSpecs) != 0 ||
		len(provider.deletedGateways) != 0 || len(provider.deletedRoutes) != 0 {
		t.Fatalf(
			"provider calls = EnsureGateway %d, EnsureRoute %d, DeleteGateway %d, DeleteRoute %d; want none",
			len(provider.gatewaySpecs), len(provider.routeSpecs), len(provider.deletedGateways), len(provider.deletedRoutes),
		)
	}

	var gotGateway gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), &gotGateway); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(&gotGateway, cfg.GatewayFinalizer()) {
		t.Fatal("Gateway binding was removed for an unsupported CRD version")
	}
	accepted := meta.FindStatusCondition(gotGateway.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionUnknown ||
		accepted.Reason != string(gatewayv1.GatewayReasonPending) {
		t.Fatalf("Gateway Accepted condition = %#v", accepted)
	}
	programmed := meta.FindStatusCondition(gotGateway.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	if programmed == nil || programmed.Status != metav1.ConditionTrue {
		t.Fatalf("Gateway Programmed condition = %#v, want the existing programmed state preserved", programmed)
	}

	var gotRoute gatewayv1.HTTPRoute
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), &gotRoute); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(&gotRoute, cfg.RouteFinalizer()) {
		t.Fatal("HTTPRoute binding was removed for an unsupported CRD version")
	}
	if len(gotRoute.Status.Parents) != 1 {
		t.Fatalf("HTTPRoute parent statuses = %#v", gotRoute.Status.Parents)
	}
	routeAccepted := meta.FindStatusCondition(gotRoute.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
	if routeAccepted == nil || routeAccepted.Status != metav1.ConditionUnknown ||
		routeAccepted.Reason != string(gatewayv1.RouteReasonPending) {
		t.Fatalf("HTTPRoute Accepted condition = %#v", routeAccepted)
	}
	routeProgrammed := meta.FindStatusCondition(gotRoute.Status.Parents[0].Conditions, cfg.Domain()+"/Programmed")
	if routeProgrammed == nil || routeProgrammed.Status != metav1.ConditionTrue || routeProgrammed.Reason != "Programmed" {
		t.Fatalf("HTTPRoute Programmed condition = %#v, want the existing programmed state preserved", routeProgrammed)
	}
}

func TestUnsupportedVersionStatusUsesLiveHTTPRouteProgrammedState(t *testing.T) {
	cfg := testConfig()
	gateway := programmedTestGateway(cfg)
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-uid"
	route.Generation = 1
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(route).
		WithObjects(unsupportedVersionGatewayClass(cfg), gateway, route).
		Build()
	var liveRoute gatewayv1.HTTPRoute
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), &liveRoute); err != nil {
		t.Fatal(err)
	}
	liveRoute.Status.Parents = []gatewayv1.RouteParentStatus{{
		ParentRef:      liveRoute.Spec.ParentRefs[0],
		ControllerName: cfg.ControllerName,
		Conditions: []metav1.Condition{Condition(
			cfg.Domain()+"/Programmed",
			metav1.ConditionTrue,
			"Programmed",
			"HTTPRoute is programmed",
			liveRoute.Generation,
		)},
	}}
	liveReader := &liveObjectOverrideReader{Client: kubeClient, route: &liveRoute}
	reconciler := &Reconciler{
		Client: kubeClient, APIReader: liveReader, Provider: &recordingProvider{},
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(route),
	}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	var got gatewayv1.HTTPRoute
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Parents) != 1 {
		t.Fatalf("HTTPRoute parent statuses = %#v", got.Status.Parents)
	}
	programmed := meta.FindStatusCondition(got.Status.Parents[0].Conditions, cfg.Domain()+"/Programmed")
	if programmed == nil || programmed.Status != metav1.ConditionTrue || programmed.Reason != "Programmed" {
		t.Fatalf("HTTPRoute Programmed condition = %#v, want live programmed state", programmed)
	}
}

func TestUnsupportedGatewayAPIVersionDoesNotCreateBindings(t *testing.T) {
	cfg := testConfig()
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-uid", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "openstack",
			Listeners:        []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}},
		},
	}
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-uid"
	route.Generation = 1
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(gateway, route).
		WithObjects(unsupportedVersionGatewayClass(cfg), gateway, route).
		Build()
	provider := &recordingProvider{}
	gatewayReconciler := &gatewaycontroller.Reconciler{
		Client: kubeClient, APIReader: kubeClient, Provider: provider,
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}
	routeReconciler := &Reconciler{
		Client: kubeClient, APIReader: kubeClient, Provider: provider,
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}

	if _, err := gatewayReconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gateway),
	}); err != nil {
		t.Fatalf("Gateway Reconcile() error = %v", err)
	}
	if _, err := routeReconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(route),
	}); err != nil {
		t.Fatalf("HTTPRoute Reconcile() error = %v", err)
	}
	var gotGateway gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), &gotGateway); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(&gotGateway, cfg.GatewayFinalizer()) || len(gotGateway.Annotations) != 0 {
		t.Fatalf("Gateway binding = finalizers %#v, annotations %#v; want none", gotGateway.Finalizers, gotGateway.Annotations)
	}
	var gotRoute gatewayv1.HTTPRoute
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), &gotRoute); err != nil {
		t.Fatal(err)
	}
	_, routeBindingPresent, err := routeReconciler.storedRouteIdentity(&gotRoute)
	if err != nil {
		t.Fatalf("storedRouteIdentity() error = %v", err)
	}
	if controllerutil.ContainsFinalizer(&gotRoute, cfg.RouteFinalizer()) || routeBindingPresent {
		t.Fatalf("HTTPRoute binding = finalizers %#v, annotations %#v; want none", gotRoute.Finalizers, gotRoute.Annotations)
	}
	if len(provider.gatewaySpecs) != 0 || len(provider.routeSpecs) != 0 {
		t.Fatalf("provider ensures = gateways %d, routes %d; want none", len(provider.gatewaySpecs), len(provider.routeSpecs))
	}
}

func TestUnsupportedGatewayAPIVersionAllowsDeletion(t *testing.T) {
	cfg := testConfig()
	now := metav1.Now()
	gateway := programmedTestGateway(cfg)
	gateway.DeletionTimestamp = &now
	route, _ := deletingBoundRoute(cfg)
	objects := append(
		[]client.Object{unsupportedVersionGatewayClass(cfg), gateway, route},
		unsupportedGatewayAPICRDs()...,
	)
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(gateway, route).
		WithObjects(objects...).
		Build()
	provider := &recordingProvider{}
	gatewayReconciler := &gatewaycontroller.Reconciler{
		Client: kubeClient, APIReader: kubeClient, Provider: provider,
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}
	routeReconciler := &Reconciler{
		Client: kubeClient, APIReader: kubeClient, Provider: provider,
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}

	if _, err := routeReconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: route.Namespace, Name: route.Name},
	}); err != nil {
		t.Fatalf("HTTPRoute Reconcile() error = %v", err)
	}
	if _, err := gatewayReconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name},
	}); err != nil {
		t.Fatalf("Gateway Reconcile() error = %v", err)
	}
	if len(provider.deletedRoutes) != 1 || len(provider.deletedGateways) != 1 {
		t.Fatalf("provider deletions = routes %d, gateways %d; want one each", len(provider.deletedRoutes), len(provider.deletedGateways))
	}
}

func TestUnsupportedGatewayAPIVersionAllowsControllerHandoffCleanup(t *testing.T) {
	cfg := testConfig()
	otherClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 2},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "other.example/controller"},
	}
	gateway := programmedTestGateway(cfg)
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-uid"
	route.Generation = 1
	(&Reconciler{Config: cfg}).applyRouteBinding(route, gateway)
	cachedClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(gateway, route).
		WithObjects(otherClass, gateway, route).
		Build()
	liveReader := liveReaderWithUnsupportedVersion(cachedClient)
	provider := &recordingProvider{}
	gatewayReconciler := &gatewaycontroller.Reconciler{
		Client: cachedClient, APIReader: liveReader, Provider: provider,
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}
	routeReconciler := &Reconciler{
		Client: cachedClient, APIReader: liveReader, Provider: provider,
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}

	routeRequest := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(route)}
	if result, err := routeReconciler.Reconcile(context.Background(), routeRequest); err != nil {
		t.Fatalf("first HTTPRoute Reconcile() error = %v", err)
	} else if result != (ctrl.Result{Requeue: true}) {
		t.Fatalf("first HTTPRoute Reconcile() result = %#v, want status checkpoint", result)
	}
	if len(provider.deletedRoutes) != 0 {
		t.Fatalf("route deletions before status checkpoint = %d, want 0", len(provider.deletedRoutes))
	}
	if _, err := routeReconciler.Reconcile(context.Background(), routeRequest); err != nil {
		t.Fatalf("second HTTPRoute Reconcile() error = %v", err)
	}
	if _, err := gatewayReconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gateway),
	}); err != nil {
		t.Fatalf("Gateway Reconcile() error = %v", err)
	}
	if len(provider.deletedRoutes) != 1 || len(provider.deletedGateways) != 1 {
		t.Fatalf("provider deletions = routes %d, gateways %d; want one each", len(provider.deletedRoutes), len(provider.deletedGateways))
	}
}

func TestVersionMismatchDoesNotTrustCachedControllerHandoff(t *testing.T) {
	cfg := testConfig()
	cachedClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 2},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "other.example/controller"},
	}
	liveClass := cachedClass.DeepCopy()
	liveClass.Spec.ControllerName = cfg.ControllerName
	gateway := programmedTestGateway(cfg)
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-uid"
	route.Generation = 1
	(&Reconciler{Config: cfg}).applyRouteBinding(route, gateway)
	cachedClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(gateway, route).
		WithObjects(cachedClass, gateway, route).
		Build()
	liveReader := &unsupportedGatewayAPIVersionReader{Client: cachedClient, gatewayClass: liveClass}
	provider := &recordingProvider{}
	gatewayReconciler := &gatewaycontroller.Reconciler{
		Client: cachedClient, APIReader: liveReader, Provider: provider,
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}
	routeReconciler := &Reconciler{
		Client: cachedClient, APIReader: liveReader, Provider: provider,
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}

	if _, err := routeReconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(route),
	}); !errors.Is(err, errHTTPRouteChanged) {
		t.Fatalf("HTTPRoute Reconcile() error = %v, want live ownership change", err)
	}
	if _, err := gatewayReconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gateway),
	}); err != nil {
		t.Fatalf("Gateway Reconcile() error = %v", err)
	}
	if len(provider.deletedRoutes) != 0 || len(provider.deletedGateways) != 0 {
		t.Fatalf("provider deletions = routes %d, gateways %d; want none", len(provider.deletedRoutes), len(provider.deletedGateways))
	}
	var gotGateway gatewayv1.Gateway
	if err := cachedClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), &gotGateway); err != nil {
		t.Fatal(err)
	}
	if !controller.GatewayHasControllerBinding(cfg, &gotGateway) {
		t.Fatal("Gateway binding was removed using a stale controller handoff")
	}
	var gotRoute gatewayv1.HTTPRoute
	if err := cachedClient.Get(context.Background(), client.ObjectKeyFromObject(route), &gotRoute); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(&gotRoute, cfg.RouteFinalizer()) {
		t.Fatal("HTTPRoute binding was removed using a stale controller handoff")
	}
}

func TestVersionMismatchDoesNotWriteStatusAfterLiveVersionRecovery(t *testing.T) {
	cfg := testConfig()
	cachedClass := unsupportedVersionGatewayClass(cfg)
	liveClass := cachedClass.DeepCopy()
	SetCondition(&liveClass.Status.Conditions, Condition(
		string(gatewayv1.GatewayClassConditionStatusSupportedVersion),
		metav1.ConditionTrue,
		string(gatewayv1.GatewayClassReasonSupportedVersion),
		"Gateway API version is supported",
		liveClass.Generation,
	))
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-uid", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "openstack",
			Listeners:        []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}},
		},
	}
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-uid"
	route.Generation = 1
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(gateway, route).
		WithObjects(cachedClass, gateway, route).
		Build()
	liveReader := &unsupportedGatewayAPIVersionReader{
		Client: kubeClient, gatewayClass: liveClass, useClientCRDs: true,
	}
	gatewayReconciler := &gatewaycontroller.Reconciler{
		Client: kubeClient, APIReader: liveReader, Provider: &recordingProvider{},
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}
	routeReconciler := &Reconciler{
		Client: kubeClient, APIReader: liveReader, Provider: &recordingProvider{},
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}

	if _, err := gatewayReconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gateway),
	}); err == nil || err.Error() != "gateway changed during reconciliation" {
		t.Fatalf("Gateway Reconcile() error = %v, want gateway changed during reconciliation", err)
	}
	if _, err := routeReconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(route),
	}); !errors.Is(err, errHTTPRouteChanged) {
		t.Fatalf("HTTPRoute Reconcile() error = %v, want %v", err, errHTTPRouteChanged)
	}
	var gotGateway gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), &gotGateway); err != nil {
		t.Fatal(err)
	}
	if len(gotGateway.Status.Conditions) != 0 {
		t.Fatalf("Gateway conditions = %#v, want none after version recovery", gotGateway.Status.Conditions)
	}
	var gotRoute gatewayv1.HTTPRoute
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), &gotRoute); err != nil {
		t.Fatal(err)
	}
	if len(gotRoute.Status.Parents) != 0 {
		t.Fatalf("HTTPRoute parent statuses = %#v, want none after version recovery", gotRoute.Status.Parents)
	}
}

func TestVersionMismatchDoesNotWriteStatusAfterLiveControllerHandoff(t *testing.T) {
	cfg := testConfig()
	cachedClass := unsupportedVersionGatewayClass(cfg)
	liveClass := cachedClass.DeepCopy()
	liveClass.Spec.ControllerName = "other.example/controller"
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-uid", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "openstack",
			Listeners:        []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}},
		},
	}
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-uid"
	route.Generation = 1
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(gateway, route).
		WithObjects(cachedClass, gateway, route).
		Build()
	liveReader := &unsupportedGatewayAPIVersionReader{
		Client: kubeClient, gatewayClass: liveClass, useClientCRDs: true,
	}
	gatewayReconciler := &gatewaycontroller.Reconciler{
		Client: kubeClient, APIReader: liveReader, Provider: &recordingProvider{},
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}
	routeReconciler := &Reconciler{
		Client: kubeClient, APIReader: liveReader, Provider: &recordingProvider{},
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}

	if _, err := gatewayReconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gateway),
	}); err != nil {
		t.Fatalf("Gateway Reconcile() error = %v", err)
	}
	if _, err := routeReconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(route),
	}); err != nil {
		t.Fatalf("HTTPRoute Reconcile() error = %v", err)
	}
	var gotGateway gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), &gotGateway); err != nil {
		t.Fatal(err)
	}
	if len(gotGateway.Status.Conditions) != 0 {
		t.Fatalf("Gateway conditions = %#v, want none after live controller handoff", gotGateway.Status.Conditions)
	}
	var gotRoute gatewayv1.HTTPRoute
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), &gotRoute); err != nil {
		t.Fatal(err)
	}
	if len(gotRoute.Status.Parents) != 0 {
		t.Fatalf("HTTPRoute parent statuses = %#v, want none after live controller handoff", gotRoute.Status.Parents)
	}
}

func TestHTTPRouteBindingChecksControllerOwnershipAgainAfterEnsure(t *testing.T) {
	cfg := testConfig()
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	foreignClass := gatewayClass.DeepCopy()
	foreignClass.Spec.ControllerName = "other.example/controller"
	gateway := programmedTestGateway(cfg)
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-uid"
	route.Generation = 1
	service, endpointSlice, node := validNodePortBackend("default", "backend")
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(route).
		WithObjects(gatewayClass, gateway, route, service, endpointSlice, node).
		Build()
	liveReader := &gatewayClassFlipReader{
		Client: kubeClient, className: gatewayClass.Name, afterFirst: foreignClass,
	}
	provider := &recordingProvider{}
	reconciler := &Reconciler{
		Client: kubeClient, APIReader: liveReader, Provider: provider,
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(route),
	})
	if !errors.Is(err, errHTTPRouteChanged) {
		t.Fatalf("Reconcile() error = %v, want %v", err, errHTTPRouteChanged)
	}
	if len(provider.routeSpecs) != 0 {
		t.Fatalf("EnsureRoute calls = %d, want 0 before the binding checkpoint", len(provider.routeSpecs))
	}
	var got gatewayv1.HTTPRoute
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), &got); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(&got, cfg.RouteFinalizer()) {
		t.Fatal("HTTPRoute received a binding after live controller handoff")
	}
}

func TestHTTPRouteProviderMutationRechecksControllerOwnership(t *testing.T) {
	cfg := testConfig()
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	foreignClass := gatewayClass.DeepCopy()
	foreignClass.Spec.ControllerName = "other.example/controller"
	gateway := programmedTestGateway(cfg)
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-uid"
	route.Generation = 1
	(&Reconciler{Config: cfg}).applyRouteBinding(route, gateway)
	service, endpointSlice, node := validNodePortBackend("default", "backend")
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(route).
		WithObjects(gatewayClass, gateway, route, service, endpointSlice, node).
		Build()
	liveReader := &gatewayClassFlipReader{
		Client: kubeClient, className: gatewayClass.Name, afterFirst: foreignClass,
	}
	provider := &recordingProvider{}
	reconciler := &Reconciler{
		Client: kubeClient, APIReader: liveReader, Provider: provider,
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}

	if _, err := reconciler.ensureRouteGraph(context.Background(), route, gateway); !errors.Is(err, errHTTPRouteChanged) {
		t.Fatalf("ensureRouteGraph() error = %v, want %v", err, errHTTPRouteChanged)
	}
	if len(provider.routeSpecs) != 0 {
		t.Fatalf("EnsureRoute calls = %d, want 0 after live controller handoff", len(provider.routeSpecs))
	}
}

func TestHTTPRouteProviderMutationRechecksGatewayBinding(t *testing.T) {
	cfg := testConfig()
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	gateway := programmedTestGateway(cfg)
	changedGateway := gateway.DeepCopy()
	changedGateway.Annotations[cfg.GatewayListenerPortAnnotation()] = "443"
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-uid"
	route.Generation = 1
	(&Reconciler{Config: cfg}).applyRouteBinding(route, gateway)
	service, endpointSlice, node := validNodePortBackend("default", "backend")
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(route).
		WithObjects(gatewayClass, gateway, route, service, endpointSlice, node).
		Build()
	liveReader := &gatewayBindingFlipReader{Client: kubeClient, afterFirst: changedGateway}
	provider := &recordingProvider{}
	reconciler := &Reconciler{
		Client: kubeClient, APIReader: liveReader, Provider: provider,
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}

	if _, err := reconciler.ensureRouteGraph(context.Background(), route, gateway); !errors.Is(err, errHTTPRouteChanged) {
		t.Fatalf("ensureRouteGraph() error = %v, want %v", err, errHTTPRouteChanged)
	}
	if len(provider.routeSpecs) != 0 {
		t.Fatalf("EnsureRoute calls = %d, want 0 after the Gateway binding changed", len(provider.routeSpecs))
	}
}

func TestHTTPRouteBindingMutationRechecksControllerOwnership(t *testing.T) {
	cfg := testConfig()
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	foreignClass := gatewayClass.DeepCopy()
	foreignClass.Spec.ControllerName = "other.example/controller"
	gateway := programmedTestGateway(cfg)
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-uid"
	route.Generation = 1
	service, endpointSlice, node := validNodePortBackend("default", "backend")
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(route).
		WithObjects(gatewayClass, gateway, route, service, endpointSlice, node).
		Build()
	liveReader := &gatewayClassFlipReader{
		Client: kubeClient, className: gatewayClass.Name, afterFirst: foreignClass,
	}
	reconciler := &Reconciler{
		Client: kubeClient, APIReader: liveReader, Provider: &recordingProvider{},
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}

	if _, err := reconciler.bindRoute(context.Background(), route, gateway); !errors.Is(err, errHTTPRouteChanged) {
		t.Fatalf("bindRoute() error = %v, want %v", err, errHTTPRouteChanged)
	}
	var got gatewayv1.HTTPRoute
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), &got); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(&got, cfg.RouteFinalizer()) {
		t.Fatal("HTTPRoute received a binding after live controller handoff")
	}
}

func TestHTTPRouteCleanupRebuildsLiveDesiredState(t *testing.T) {
	tests := []struct {
		name            string
		mutateCached    func(*gatewayv1.Gateway, *corev1.Service)
		overrideGateway bool
		overrideService bool
	}{
		{
			name: "Gateway listener recovered",
			mutateCached: func(gateway *gatewayv1.Gateway, _ *corev1.Service) {
				gateway.Spec.Listeners[0].Protocol = gatewayv1.HTTPSProtocolType
			},
			overrideGateway: true,
		},
		{
			name: "backend Service recovered",
			mutateCached: func(_ *gatewayv1.Gateway, service *corev1.Service) {
				service.Spec.Type = corev1.ServiceTypeClusterIP
			},
			overrideService: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig()
			gatewayClass := &gatewayv1.GatewayClass{
				ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 1},
				Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
			}
			liveGateway := programmedTestGateway(cfg)
			cachedGateway := liveGateway.DeepCopy()
			liveService, endpointSlice, node := validNodePortBackend("default", "backend")
			cachedService := liveService.DeepCopy()
			test.mutateCached(cachedGateway, cachedService)
			route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
			route.UID = "route-uid"
			route.Generation = 1
			(&Reconciler{Config: cfg}).applyRouteBinding(route, liveGateway)
			kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
				WithStatusSubresource(route).
				WithObjects(gatewayClass, cachedGateway, route, cachedService, endpointSlice, node).
				Build()
			liveReader := &liveObjectOverrideReader{Client: kubeClient}
			if test.overrideGateway {
				liveReader.gateway = liveGateway
			}
			if test.overrideService {
				liveReader.service = liveService
			}
			provider := &recordingProvider{}
			reconciler := &Reconciler{
				Client: kubeClient, APIReader: liveReader, Provider: provider,
				Coordinator: &graph.Coordinator{}, Config: cfg,
			}

			request := ctrl.Request{
				NamespacedName: client.ObjectKeyFromObject(route),
			}
			result, err := reconciler.Reconcile(context.Background(), request)
			if err != nil {
				t.Fatalf("first Reconcile() error = %v", err)
			}
			if result != (ctrl.Result{Requeue: true}) {
				t.Fatalf("first Reconcile() result = %#v, want status checkpoint", result)
			}
			if len(provider.deletedRoutes) != 0 {
				t.Fatalf("DeleteRoute calls before status checkpoint = %d, want 0", len(provider.deletedRoutes))
			}

			_, err = reconciler.Reconcile(context.Background(), request)
			if !errors.Is(err, errHTTPRouteChanged) {
				t.Fatalf("second Reconcile() error = %v, want %v", err, errHTTPRouteChanged)
			}
			if len(provider.deletedRoutes) != 0 {
				t.Fatalf("DeleteRoute calls = %d, want 0", len(provider.deletedRoutes))
			}
			var got gatewayv1.HTTPRoute
			if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), &got); err != nil {
				t.Fatal(err)
			}
			if !controllerutil.ContainsFinalizer(&got, cfg.RouteFinalizer()) {
				t.Fatal("HTTPRoute binding was removed using stale dependency state")
			}
		})
	}
}

func validNodePortBackend(namespace, name string) (*corev1.Service, *discoveryv1.EndpointSlice, *corev1.Node) {
	ready := true
	nodeName := "worker-0"
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{{Port: 80, NodePort: 30080, Protocol: corev1.ProtocolTCP}},
		},
	}
	endpointSlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name + "-1",
			Labels:    map[string]string{discoveryv1.LabelServiceName: name},
		},
		Endpoints: []discoveryv1.Endpoint{{
			Conditions: discoveryv1.EndpointConditions{Ready: &ready},
			NodeName:   &nodeName,
		}},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Status: corev1.NodeStatus{
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.10"}},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	return service, endpointSlice, node
}

func unsupportedVersionGatewayClass(cfg Config) *gatewayv1.GatewayClass {
	return &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
		Status: gatewayv1.GatewayClassStatus{Conditions: []metav1.Condition{Condition(
			string(gatewayv1.GatewayClassConditionStatusSupportedVersion),
			metav1.ConditionFalse,
			string(gatewayv1.GatewayClassReasonUnsupportedVersion),
			"unsupported test bundle",
			1,
		)}},
	}
}

func unsupportedGatewayAPICRDs() []client.Object {
	definitions := supportedGatewayAPICRDs()
	for _, definition := range definitions {
		if definition.GetName() == "httproutes.gateway.networking.k8s.io" {
			definition.SetAnnotations(map[string]string{gatewayconsts.BundleVersionAnnotation: "v1.5.0"})
		}
	}
	return definitions
}

type gatewayClassFlipReader struct {
	client.Client
	mu         sync.Mutex
	className  string
	classGets  int
	afterFirst *gatewayv1.GatewayClass
}

type gatewayBindingFlipReader struct {
	client.Client
	mu          sync.Mutex
	gatewayGets int
	afterFirst  *gatewayv1.Gateway
}

func (r *gatewayBindingFlipReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	opts ...client.GetOption,
) error {
	if gateway, ok := object.(*gatewayv1.Gateway); ok && key == client.ObjectKeyFromObject(r.afterFirst) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.gatewayGets++
		if r.gatewayGets > 1 {
			r.afterFirst.DeepCopyInto(gateway)
			return nil
		}
	}
	return r.Client.Get(ctx, key, object, opts...)
}

func (r *gatewayClassFlipReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	opts ...client.GetOption,
) error {
	if gatewayClass, ok := object.(*gatewayv1.GatewayClass); ok && key.Name == r.className {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.classGets++
		if r.classGets > 1 {
			r.afterFirst.DeepCopyInto(gatewayClass)
			return nil
		}
	}
	return r.Client.Get(ctx, key, object, opts...)
}

type liveObjectOverrideReader struct {
	client.Client
	gateway *gatewayv1.Gateway
	service *corev1.Service
	route   *gatewayv1.HTTPRoute
}

func (r *liveObjectOverrideReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	opts ...client.GetOption,
) error {
	switch current := object.(type) {
	case *gatewayv1.Gateway:
		if r.gateway != nil && key == client.ObjectKeyFromObject(r.gateway) {
			r.gateway.DeepCopyInto(current)
			return nil
		}
	case *corev1.Service:
		if r.service != nil && key == client.ObjectKeyFromObject(r.service) {
			r.service.DeepCopyInto(current)
			return nil
		}
	case *gatewayv1.HTTPRoute:
		if r.route != nil && key == client.ObjectKeyFromObject(r.route) {
			r.route.DeepCopyInto(current)
			return nil
		}
	}
	return r.Client.Get(ctx, key, object, opts...)
}

type unsupportedGatewayAPIVersionReader struct {
	client.Client
	gatewayClass  *gatewayv1.GatewayClass
	useClientCRDs bool
}

func (r *unsupportedGatewayAPIVersionReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	opts ...client.GetOption,
) error {
	gatewayClass, ok := object.(*gatewayv1.GatewayClass)
	if ok && r.gatewayClass != nil && key.Name == r.gatewayClass.Name {
		r.gatewayClass.DeepCopyInto(gatewayClass)
		return nil
	}
	return r.Client.Get(ctx, key, object, opts...)
}

func (r *unsupportedGatewayAPIVersionReader) List(
	ctx context.Context,
	list client.ObjectList,
	opts ...client.ListOption,
) error {
	definitions, ok := list.(*apiextensionsv1.CustomResourceDefinitionList)
	metadata, metadataOK := list.(*metav1.PartialObjectMetadataList)
	if (!ok && !metadataOK) || r.useClientCRDs {
		return r.Client.List(ctx, list, opts...)
	}
	if ok {
		definitions.Items = nil
	}
	if metadataOK {
		metadata.Items = nil
	}
	for _, object := range unsupportedGatewayAPICRDs() {
		definition := object.(*apiextensionsv1.CustomResourceDefinition)
		if ok {
			definitions.Items = append(definitions.Items, *definition.DeepCopy())
		}
		if metadataOK {
			metadata.Items = append(metadata.Items, metav1.PartialObjectMetadata{
				ObjectMeta: *definition.ObjectMeta.DeepCopy(),
			})
		}
	}
	return nil
}

func liveReaderWithUnsupportedVersion(kubeClient client.Client) client.Reader {
	return &unsupportedGatewayAPIVersionReader{Client: kubeClient}
}
