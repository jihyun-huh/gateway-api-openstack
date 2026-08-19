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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
	gatewaycontroller "github.com/jihyun-huh/gateway-api-openstack/internal/controller/gateway"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller/graph"
)

func routeBindingFinalizers(cfg Config, gatewayNamespace, gatewayName, gatewayUID string) []string {
	return []string{
		cfg.RouteFinalizer(),
		cfg.RouteBindingFinalizer(cfg.ClusterID, cfg.OpenStackProjectID, gatewayNamespace, gatewayName, gatewayUID),
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
				cfg.RouteGatewayNamespaceAnnotation(): "default",
				cfg.RouteGatewayNameAnnotation():      "old-gateway",
				cfg.RouteGatewayUIDAnnotation():       "old-gateway-uid",
			},
		},
	}
	client := indexedFakeClientBuilder(scheme, testConfig()).WithStatusSubresource(route).WithObjects(route).Build()
	provider := &recordingProvider{}
	reconciler := Reconciler{Client: client, Provider: provider, Coordinator: &graph.Coordinator{}, APIReader: client, Config: cfg}
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
				cfg.RouteGatewayNamespaceAnnotation(): "default",
				cfg.RouteGatewayNameAnnotation():      "different-gateway",
				cfg.RouteGatewayUIDAnnotation():       "different-gateway-uid",
			},
		},
	}
	kubeClient := indexedFakeClientBuilder(scheme, testConfig()).WithStatusSubresource(route).WithObjects(route).Build()
	provider := &recordingProvider{}
	reconciler := Reconciler{Client: kubeClient, Provider: provider, Config: cfg}
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
	if !controllerutil.ContainsFinalizer(&got, cfg.RouteFinalizer()) {
		t.Fatal("tampered binding removed the safety finalizer")
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
	kubeClient := indexedFakeClientBuilder(scheme, testConfig()).WithObjects(gateway, route).Build()
	reconciler := Reconciler{Client: kubeClient, Config: testConfig()}
	requests := reconciler.enqueueHTTPRoutesForGateway(context.Background(), gateway)
	if len(requests) != 1 || requests[0].NamespacedName != (types.NamespacedName{Namespace: route.Namespace, Name: route.Name}) {
		t.Fatalf("enqueueHTTPRoutesForGateway() = %#v", requests)
	}
}

func TestHTTPRouteReconcileBuildsNodePortMembers(t *testing.T) {
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
	client := indexedFakeClientBuilder(scheme, testConfig()).WithStatusSubresource(route).WithObjects(class, gateway, route, service, slice, node).Build()
	provider := &recordingProvider{}
	reconciler := Reconciler{Client: client, Provider: provider, Coordinator: &graph.Coordinator{}, APIReader: client, Config: cfg}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "api"}}
	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result != (ctrl.Result{Requeue: true}) || provider.gatewayGets != 0 || len(provider.routeSpecs) != 0 {
		t.Fatalf("binding checkpoint result = %#v, GetGateway calls = %d, EnsureRoute calls = %d", result, provider.gatewayGets, len(provider.routeSpecs))
	}
	secondResult, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	wantResync := controller.OpenStackResyncAfter(cfg.OpenStackResyncInterval, route.UID)
	if secondResult.RequeueAfter != wantResync {
		t.Fatalf("second Reconcile() RequeueAfter = %s, want %s", secondResult.RequeueAfter, wantResync)
	}
	if len(provider.routeSpecs) != 1 {
		var current gatewayv1.HTTPRoute
		_ = client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "api"}, &current)
		t.Fatalf("route specs count = %d, status = %#v", len(provider.routeSpecs), current.Status)
	}
	got := provider.routeSpecs[0]
	wantGatewayRequirements := cloud.GatewayRequirements{
		Provider: cfg.Provider, VIPSubnetID: cfg.VIPSubnetID,
		ExternalNetworkID: cfg.ExternalNetworkID, ListenerPort: 80,
	}
	if got.GatewayRequirements != wantGatewayRequirements || got.PathType != cloud.PathMatchPrefix || got.PathValue != "/api" ||
		len(got.Members) != 1 || got.Members[0] != (cloud.Member{Address: "10.0.0.11", Port: 30080}) {
		t.Fatalf("route spec = %#v", got)
	}
	var reconciled gatewayv1.HTTPRoute
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "api"}, &reconciled); err != nil {
		t.Fatal(err)
	}
	if reconciled.Annotations[cfg.RouteGatewayUIDAnnotation()] != "gateway-uid" {
		t.Fatalf("stored Gateway UID = %q", reconciled.Annotations[cfg.RouteGatewayUIDAnnotation()])
	}
	resourceVersion := reconciled.ResourceVersion
	thirdResult, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("third Reconcile() error = %v", err)
	}
	if thirdResult.RequeueAfter != wantResync || len(provider.routeSpecs) != 2 {
		t.Fatalf("third Reconcile() result = %#v, EnsureRoute calls = %d", thirdResult, len(provider.routeSpecs))
	}
	if err := client.Get(context.Background(), request.NamespacedName, &reconciled); err != nil {
		t.Fatal(err)
	}
	if reconciled.ResourceVersion != resourceVersion {
		t.Fatalf("converged resync changed HTTPRoute resourceVersion from %s to %s", resourceVersion, reconciled.ResourceVersion)
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
				cfg.RouteGatewayNamespaceAnnotation(): "default",
				cfg.RouteGatewayNameAnnotation():      "old-gateway",
				cfg.RouteGatewayUIDAnnotation():       "old-gateway-uid",
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
	kubeClient := indexedFakeClientBuilder(scheme, testConfig()).WithStatusSubresource(route).WithObjects(route).Build()
	provider := &recordingProvider{}
	reconciler := Reconciler{Client: kubeClient, Provider: provider, Coordinator: &graph.Coordinator{}, APIReader: kubeClient, Config: cfg}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: route.Namespace, Name: route.Name}}
	if result, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	} else if result != (ctrl.Result{Requeue: true}) {
		t.Fatalf("first Reconcile() result = %#v, want status checkpoint", result)
	}
	if len(provider.deletedRoutes) != 0 {
		t.Fatalf("DeleteRoute calls before status checkpoint = %d, want 0", len(provider.deletedRoutes))
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if len(provider.deletedRoutes) != 1 || provider.deletedRoutes[0].GatewayUID != "old-gateway-uid" {
		t.Fatalf("deleted route identities = %#v", provider.deletedRoutes)
	}
	var got gatewayv1.HTTPRoute
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: route.Namespace, Name: route.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Finalizers) != 0 || got.Annotations[cfg.RouteGatewayUIDAnnotation()] != "" {
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
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "new-gateway", UID: "new-gateway-uid", Generation: 1,
			Finalizers:  []string{cfg.GatewayFinalizer()},
			Annotations: gatewayBindingAnnotations(cfg, "80"),
		},
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
				cfg.RouteGatewayNamespaceAnnotation(): "default",
				cfg.RouteGatewayNameAnnotation():      "old-gateway",
				cfg.RouteGatewayUIDAnnotation():       "old-gateway-uid",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "new-gateway"}}},
			Rules:           []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "backend", Port: portNumberPointer(80)}}}}}},
		},
	}
	kubeClient := indexedFakeClientBuilder(scheme, testConfig()).WithStatusSubresource(route).WithObjects(class, newGateway, route).Build()
	provider := &recordingProvider{}
	reconciler := Reconciler{Client: kubeClient, Provider: provider, Coordinator: &graph.Coordinator{}, APIReader: kubeClient, Config: cfg}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: route.Namespace, Name: route.Name}}
	if result, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	} else if result != (ctrl.Result{Requeue: true}) {
		t.Fatalf("first Reconcile() result = %#v, want status checkpoint", result)
	}
	if len(provider.deletedRoutes) != 0 {
		t.Fatalf("DeleteRoute calls before status checkpoint = %d, want 0", len(provider.deletedRoutes))
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if len(provider.deletedRoutes) != 1 || provider.deletedRoutes[0].GatewayUID != "old-gateway-uid" {
		t.Fatalf("deleted route identities = %#v", provider.deletedRoutes)
	}
	var got gatewayv1.HTTPRoute
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: route.Namespace, Name: route.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(&got, cfg.RouteFinalizer()) || got.Annotations[cfg.RouteGatewayUIDAnnotation()] != "" {
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
	kubeClient := indexedFakeClientBuilder(scheme, testConfig()).WithStatusSubresource(route).WithObjects(class, gateway, route).Build()
	provider := &recordingProvider{}
	reconciler := Reconciler{Client: kubeClient, Provider: provider, Coordinator: &graph.Coordinator{}, APIReader: kubeClient, Config: cfg}
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
	kubeClient := indexedFakeClientBuilder(scheme, testConfig()).WithObjects(oldRoute, current).Build()
	gateway := programmedTestGateway(testConfig())
	reconciler := Reconciler{Client: kubeClient, Config: testConfig()}
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
	kubeClient := indexedFakeClientBuilder(scheme, testConfig()).WithObjects(oldRoute, current, clusterIP, nodePort).Build()
	gateway := programmedTestGateway(testConfig())
	reconciler := Reconciler{Client: kubeClient, Config: testConfig()}
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
			kubeClient := indexedFakeClientBuilder(testScheme(t), testConfig()).WithObjects(service).Build()
			reconciler := Reconciler{Client: kubeClient, Config: testConfig()}
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
	kubeClient := indexedFakeClientBuilder(scheme, testConfig()).
		WithStatusSubresource(&gatewayv1.Gateway{}, &gatewayv1.HTTPRoute{}).
		WithObjects(class, gateway, terminalRoute, validRoute, udpService, tcpService, slice, node).
		Build()
	provider := &recordingProvider{}
	reconciler := Reconciler{Client: kubeClient, Provider: provider, Coordinator: &graph.Coordinator{}, APIReader: kubeClient, Config: cfg}
	ctx := context.Background()
	for _, route := range []*gatewayv1.HTTPRoute{terminalRoute, validRoute} {
		if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(route)}); err != nil {
			t.Fatalf("Reconcile(%s) error = %v", route.Name, err)
		}
	}
	if len(provider.routeSpecs) != 0 {
		t.Fatalf("EnsureRoute calls before binding checkpoint = %d, want 0", len(provider.routeSpecs))
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(validRoute)}); err != nil {
		t.Fatalf("second Reconcile(%s) error = %v", validRoute.Name, err)
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
	programmed := meta.FindStatusCondition(gotValid.Status.Parents[0].Conditions, cfg.Domain()+"/Programmed")
	if programmed == nil || programmed.Status != metav1.ConditionTrue {
		t.Fatalf("valid route Programmed condition = %#v", programmed)
	}
	if len(provider.routeSpecs) != 1 || provider.routeSpecs[0].Identity.RouteName != validRoute.Name {
		t.Fatalf("EnsureRoute specs = %#v", provider.routeSpecs)
	}

	gatewayReconciler := gatewaycontroller.Reconciler{
		Client: kubeClient, APIReader: kubeClient, Provider: provider,
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}
	if _, err := gatewayReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gateway)}); err != nil {
		t.Fatalf("Gateway Reconcile() error = %v", err)
	}
	var gotGateway gatewayv1.Gateway
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(gateway), &gotGateway); err != nil {
		t.Fatal(err)
	}
	if len(gotGateway.Status.Listeners) != 1 || gotGateway.Status.Listeners[0].AttachedRoutes != 1 {
		t.Fatalf("Gateway listener status = %#v, want one attached route", gotGateway.Status.Listeners)
	}
}

func TestBindRouteRefetchesBeforePatchingMetadata(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-uid"
	route.Generation = 1
	gateway := programmedTestGateway(cfg)
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: string(gateway.Spec.GatewayClassName), Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	service, endpointSlice, node := validNodePortBackend("default", "backend")
	kubeClient := indexedFakeClientBuilder(scheme, testConfig()).
		WithObjects(route, gateway, gatewayClass, service, endpointSlice, node).
		Build()

	staleRoute := route.DeepCopy()
	current := &gatewayv1.HTTPRoute{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	current.Annotations = map[string]string{"example.com/concurrent": "preserve"}
	if err := kubeClient.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}

	reconciler := Reconciler{Client: kubeClient, APIReader: kubeClient, Provider: &recordingProvider{}, Config: cfg}
	bindingStored, err := reconciler.bindRoute(context.Background(), staleRoute, gateway)
	if err != nil {
		t.Fatalf("bindRoute() error = %v", err)
	}
	if !bindingStored {
		t.Fatal("bindRoute() did not report the metadata checkpoint")
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	if current.Annotations["example.com/concurrent"] != "preserve" {
		t.Fatalf("concurrent annotation was lost: %#v", current.Annotations)
	}
	if current.Annotations[cfg.RouteGatewayUIDAnnotation()] != string(gateway.UID) ||
		!controllerutil.ContainsFinalizer(current, cfg.RouteFinalizer()) {
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
	kubeClient := indexedFakeClientBuilder(scheme, testConfig()).WithObjects(current).Build()
	staleRoute := current.DeepCopy()
	staleRoute.Generation = 1

	reconciler := Reconciler{Client: kubeClient, APIReader: kubeClient, Provider: &recordingProvider{}, Config: cfg}
	if _, err := reconciler.bindRoute(context.Background(), staleRoute, programmedTestGateway(cfg)); !errors.Is(err, errHTTPRouteChanged) {
		t.Fatalf("bindRoute() error = %v, want %v", err, errHTTPRouteChanged)
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(current), current); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(current, cfg.RouteFinalizer()) {
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
			cfg.RouteGatewayNamespaceAnnotation(): "default",
			cfg.RouteGatewayNameAnnotation():      "edge",
			cfg.RouteGatewayUIDAnnotation():       "gateway-uid",
		},
	}}
	kubeClient := indexedFakeClientBuilder(scheme, testConfig()).WithObjects(route).Build()
	staleRoute := route.DeepCopy()
	staleRoute.Generation = 1
	provider := &recordingProvider{}
	reconciler := Reconciler{Client: kubeClient, Provider: provider, Config: cfg}

	if _, err := reconciler.detachRoute(context.Background(), staleRoute); !errors.Is(err, errHTTPRouteChanged) {
		t.Fatalf("detachRoute() error = %v, want %v", err, errHTTPRouteChanged)
	}
	if len(provider.deletedRoutes) != 0 {
		t.Fatalf("DeleteRoute calls = %d, want 0", len(provider.deletedRoutes))
	}
	current := &gatewayv1.HTTPRoute{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(current, cfg.RouteFinalizer()) {
		t.Fatal("stale reconciliation removed the HTTPRoute finalizer")
	}
}

func TestDetachRouteDoesNotRepeatCloudDeleteAfterPatchConflict(t *testing.T) {
	scheme := testScheme(t)
	cfg := testConfig()
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Namespace:  "default",
		Name:       "api",
		UID:        "route-uid",
		Generation: 1,
		Finalizers: routeBindingFinalizers(cfg, "default", "edge", "gateway-uid"),
		Annotations: map[string]string{
			cfg.RouteGatewayNamespaceAnnotation(): "default",
			cfg.RouteGatewayNameAnnotation():      "edge",
			cfg.RouteGatewayUIDAnnotation():       "gateway-uid",
		},
	}}
	baseClient := indexedFakeClientBuilder(scheme, cfg).WithObjects(route).Build()
	cachedRoute := &gatewayv1.HTTPRoute{}
	if err := baseClient.Get(context.Background(), client.ObjectKeyFromObject(route), cachedRoute); err != nil {
		t.Fatal(err)
	}
	kubeClient := &conflictOncePatchClient{Client: baseClient, cachedRoute: cachedRoute}
	provider := &recordingProvider{}
	reconciler := Reconciler{
		Client:      kubeClient,
		APIReader:   baseClient,
		Provider:    provider,
		Coordinator: &graph.Coordinator{},
		Config:      cfg,
	}

	if _, err := reconciler.detachRoute(context.Background(), route.DeepCopy()); err != nil {
		t.Fatalf("detachRoute() error = %v", err)
	}
	if len(provider.deletedRoutes) != 1 {
		t.Fatalf("DeleteRoute calls = %d, want 1", len(provider.deletedRoutes))
	}
	if kubeClient.patchCalls != 2 {
		t.Fatalf("Patch calls = %d, want 2", kubeClient.patchCalls)
	}
	current := &gatewayv1.HTTPRoute{}
	if err := baseClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	if current.Annotations["example.com/concurrent"] != "preserve" {
		t.Fatalf("concurrent annotation = %q, want preserve", current.Annotations["example.com/concurrent"])
	}
	if controllerutil.ContainsFinalizer(current, cfg.RouteFinalizer()) {
		t.Fatal("HTTPRoute finalizer was not removed")
	}
	if _, present, err := reconciler.storedRouteIdentity(current); err != nil || present {
		t.Fatalf("storedRouteIdentity() = present %t, error %v; want no stored binding", present, err)
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
	kubeClient := indexedFakeClientBuilder(scheme, testConfig()).
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

	reconciler := Reconciler{Client: kubeClient, Config: cfg}
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

type conflictOncePatchClient struct {
	client.Client
	cachedRoute *gatewayv1.HTTPRoute
	patchCalls  int
}

func (c *conflictOncePatchClient) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	route, ok := object.(*gatewayv1.HTTPRoute)
	if ok && c.cachedRoute != nil && key == client.ObjectKeyFromObject(c.cachedRoute) {
		candidate := c.cachedRoute.DeepCopy()
		*route = *candidate
		return nil
	}
	return c.Client.Get(ctx, key, object, options...)
}

func (c *conflictOncePatchClient) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	c.patchCalls++
	if c.patchCalls == 1 {
		current := &gatewayv1.HTTPRoute{}
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil {
			return err
		}
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations["example.com/concurrent"] = "preserve"
		if err := c.Update(ctx, current); err != nil {
			return err
		}
		return apierrors.NewConflict(
			schema.GroupResource{Group: gatewayv1.GroupName, Resource: "httproutes"},
			object.GetName(),
			errors.New("injected metadata conflict"),
		)
	}
	return c.Client.Patch(ctx, object, patch, options...)
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
	kubeClient := indexedFakeClientBuilder(scheme, testConfig()).WithObjects(objects...).Build()
	reconciler := Reconciler{Client: kubeClient, Config: testConfig()}
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
