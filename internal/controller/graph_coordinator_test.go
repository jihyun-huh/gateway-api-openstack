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
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

func TestGraphCoordinatorSerializesSameGateway(t *testing.T) {
	coordinator := &GraphCoordinator{}
	uid := types.UID("gateway-1")

	releaseFirst, err := coordinator.Acquire(context.Background(), uid)
	if err != nil {
		t.Fatalf("acquire first graph lock: %v", err)
	}

	acquiredSecond := make(chan func(), 1)
	errSecond := make(chan error, 1)
	go func() {
		release, err := coordinator.Acquire(context.Background(), uid)
		if err != nil {
			errSecond <- err
			return
		}
		acquiredSecond <- release
	}()

	waitForGraphReferences(t, coordinator, uid, 2)
	select {
	case release := <-acquiredSecond:
		release()
		t.Fatal("second caller acquired the same Gateway before release")
	case err := <-errSecond:
		t.Fatalf("acquire second graph lock: %v", err)
	default:
	}

	releaseFirst()
	select {
	case release := <-acquiredSecond:
		release()
	case err := <-errSecond:
		t.Fatalf("acquire second graph lock: %v", err)
	case <-time.After(time.Second):
		t.Fatal("second caller did not acquire the released Gateway")
	}

	waitForGraphReferences(t, coordinator, uid, 0)
}

func TestGraphCoordinatorAllowsDifferentGateways(t *testing.T) {
	coordinator := &GraphCoordinator{}

	releaseFirst, err := coordinator.Acquire(context.Background(), types.UID("gateway-1"))
	if err != nil {
		t.Fatalf("acquire first Gateway: %v", err)
	}
	defer releaseFirst()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	releaseSecond, err := coordinator.Acquire(ctx, types.UID("gateway-2"))
	if err != nil {
		t.Fatalf("acquire different Gateway: %v", err)
	}
	releaseSecond()
}

func TestGraphCoordinatorRemovesCanceledWaiter(t *testing.T) {
	coordinator := &GraphCoordinator{}
	uid := types.UID("gateway-1")

	release, err := coordinator.Acquire(context.Background(), uid)
	if err != nil {
		t.Fatalf("acquire graph lock: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	waiterResult := make(chan error, 1)
	go func() {
		_, err := coordinator.Acquire(ctx, uid)
		waiterResult <- err
	}()

	waitForGraphReferences(t, coordinator, uid, 2)
	cancel()
	select {
	case err := <-waiterResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Acquire() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter did not return")
	}

	waitForGraphReferences(t, coordinator, uid, 1)
	release()
	waitForGraphReferences(t, coordinator, uid, 0)
}

func TestGraphCoordinatorReleaseIsIdempotent(t *testing.T) {
	coordinator := &GraphCoordinator{}
	uid := types.UID("gateway-1")

	release, err := coordinator.Acquire(context.Background(), uid)
	if err != nil {
		t.Fatalf("acquire graph lock: %v", err)
	}

	var waitGroup sync.WaitGroup
	for range 10 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			release()
		}()
	}
	waitGroup.Wait()
	waitForGraphReferences(t, coordinator, uid, 0)

	releaseAgain, err := coordinator.Acquire(context.Background(), uid)
	if err != nil {
		t.Fatalf("acquire graph lock after repeated release: %v", err)
	}
	releaseAgain()
}

func TestGraphCoordinatorRejectsEmptyGatewayUID(t *testing.T) {
	coordinator := &GraphCoordinator{}

	_, err := coordinator.Acquire(context.Background(), "")
	if !errors.Is(err, errEmptyGatewayUID) {
		t.Fatalf("Acquire() error = %v, want %v", err, errEmptyGatewayUID)
	}
}

func TestAcquireGatewayGraphRequiresCoordinator(t *testing.T) {
	_, err := acquireGatewayGraph(context.Background(), nil, "gateway-1")
	if !errors.Is(err, errGraphCoordinatorRequired) {
		t.Fatalf("acquireGatewayGraph() error = %v, want %v", err, errGraphCoordinatorRequired)
	}
}

func TestGraphCoordinatorDoesNotRetainCanceledContext(t *testing.T) {
	coordinator := &GraphCoordinator{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := coordinator.Acquire(ctx, types.UID("gateway-1"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire() error = %v, want context.Canceled", err)
	}
	if got := graphReferenceCount(coordinator, types.UID("gateway-1")); got != 0 {
		t.Fatalf("reference count = %d, want 0", got)
	}
}

func TestReconcilersShareGatewayGraphCoordinator(t *testing.T) {
	config := testConfig()
	coordinator := &GraphCoordinator{}
	provider := newBlockingGraphProvider()
	class := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: config.ControllerName},
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "default",
			Name:        "edge",
			UID:         "gateway-1",
			Generation:  1,
			Finalizers:  []string{config.gatewayFinalizer()},
			Annotations: gatewayBindingAnnotations(config, "80"),
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "openstack",
			Listeners:        []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}},
		},
	}
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-1"
	route.Generation = 1
	routeBinder := &HTTPRouteReconciler{Config: config}
	routeBinder.applyRouteBinding(route, gateway)
	kubeClient := indexedFakeClientBuilder(testScheme(t), config).WithObjects(class, gateway, route).Build()
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), gateway); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), route); err != nil {
		t.Fatal(err)
	}
	gatewayReconciler := &GatewayReconciler{Client: kubeClient, Provider: provider, Coordinator: coordinator, APIReader: kubeClient, Config: config}
	routeReconciler := &HTTPRouteReconciler{Client: kubeClient, Provider: provider, Coordinator: coordinator, APIReader: kubeClient, Config: config}
	storedRouteIdentity, present, err := routeReconciler.storedRouteIdentity(route)
	if err != nil || !present {
		t.Fatalf("stored route identity = %#v, present %t, error %v", storedRouteIdentity, present, err)
	}
	gatewayUID := string(gateway.UID)

	gatewayDone := make(chan error, 1)
	go func() {
		_, err := gatewayReconciler.ensureGateway(context.Background(), gateway.DeepCopy())
		gatewayDone <- err
	}()
	select {
	case <-provider.gatewayStarted:
	case <-time.After(time.Second):
		t.Fatal("Gateway mutation did not start")
	}

	routeDone := make(chan error, 1)
	go func() {
		routeDone <- routeReconciler.deleteRoute(context.Background(), route.DeepCopy(), storedRouteIdentity)
	}()
	waitForGraphReferences(t, coordinator, types.UID(gatewayUID), 2)
	select {
	case <-provider.routeObserved:
		t.Fatal("HTTPRoute observed the graph while Gateway mutation was active")
	default:
	}

	close(provider.allowGateway)
	select {
	case err := <-gatewayDone:
		if err != nil {
			t.Fatalf("ensure Gateway graph: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Gateway mutation did not finish")
	}
	select {
	case <-provider.routeObserved:
	case <-time.After(time.Second):
		t.Fatal("HTTPRoute did not observe the graph after Gateway mutation completed")
	}
	select {
	case err := <-routeDone:
		if err != nil {
			t.Fatalf("delete HTTPRoute graph: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTPRoute reconciliation did not finish")
	}
	waitForGraphReferences(t, coordinator, types.UID(gatewayUID), 0)
}

func TestGatewayRevalidatesFinalizerAfterWaitingForGraph(t *testing.T) {
	config := testConfig()
	coordinator := &GraphCoordinator{}
	class := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: config.ControllerName},
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "default",
			Name:        "edge",
			UID:         "gateway-1",
			Generation:  1,
			Finalizers:  []string{config.gatewayFinalizer()},
			Annotations: gatewayBindingAnnotations(config, "80"),
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(class.Name),
			Listeners:        []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}},
		},
	}
	kubeClient := indexedFakeClientBuilder(testScheme(t), config).WithObjects(class, gateway).Build()
	provider := &recordingProvider{}
	reconciler := &GatewayReconciler{Client: kubeClient, Provider: provider, Coordinator: coordinator, Config: config}

	release, err := coordinator.Acquire(context.Background(), gateway.UID)
	if err != nil {
		t.Fatalf("acquire graph lock: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := reconciler.ensureGateway(context.Background(), gateway.DeepCopy())
		result <- err
	}()
	waitForGraphReferences(t, coordinator, gateway.UID, 2)

	var changed gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), &changed); err != nil {
		t.Fatal(err)
	}
	changed.Finalizers = nil
	if err := kubeClient.Update(context.Background(), &changed); err != nil {
		t.Fatal(err)
	}
	release()

	if err := <-result; !errors.Is(err, errGatewayChanged) {
		t.Fatalf("ensureGateway() error = %v, want %v", err, errGatewayChanged)
	}
	if len(provider.gatewaySpecs) != 0 {
		t.Fatalf("EnsureGateway calls = %d, want 0", len(provider.gatewaySpecs))
	}
}

func TestHTTPRouteRevalidatesSnapshotAfterWaitingForGraph(t *testing.T) {
	config := testConfig()
	coordinator := &GraphCoordinator{}
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-1", Generation: 1}}
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-1"
	route.Generation = 1
	kubeClient := indexedFakeClientBuilder(testScheme(t), config).WithObjects(gateway, route).Build()
	provider := &recordingProvider{}
	reconciler := &HTTPRouteReconciler{Client: kubeClient, Provider: provider, Coordinator: coordinator, APIReader: kubeClient, Config: config}

	release, err := coordinator.Acquire(context.Background(), gateway.UID)
	if err != nil {
		t.Fatalf("acquire graph lock: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := reconciler.ensureRouteGraph(context.Background(), route.DeepCopy(), gateway.DeepCopy())
		result <- err
	}()
	waitForGraphReferences(t, coordinator, gateway.UID, 2)

	var changed gatewayv1.HTTPRoute
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), &changed); err != nil {
		t.Fatal(err)
	}
	changed.Generation = 2
	changed.Spec.Hostnames = []gatewayv1.Hostname{"new.example.test"}
	if err := kubeClient.Update(context.Background(), &changed); err != nil {
		t.Fatal(err)
	}
	release()

	if err := <-result; !errors.Is(err, errHTTPRouteChanged) {
		t.Fatalf("ensureRouteGraph() error = %v, want %v", err, errHTTPRouteChanged)
	}
	if len(provider.routeSpecs) != 0 {
		t.Fatalf("EnsureRoute calls = %d, want 0", len(provider.routeSpecs))
	}
}

func TestHTTPRouteDeleteRevalidatesBindingAfterWaitingForGraph(t *testing.T) {
	config := testConfig()
	coordinator := &GraphCoordinator{}
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-1"}}
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-1"
	route.Generation = 1
	binder := &HTTPRouteReconciler{Config: config}
	binder.applyRouteBinding(route, gateway)
	kubeClient := indexedFakeClientBuilder(testScheme(t), config).WithObjects(route).Build()
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), route); err != nil {
		t.Fatal(err)
	}
	provider := &recordingProvider{}
	reconciler := &HTTPRouteReconciler{Client: kubeClient, Provider: provider, Coordinator: coordinator, APIReader: kubeClient, Config: config}
	stored, present, err := reconciler.storedRouteIdentity(route)
	if err != nil || !present {
		t.Fatalf("stored route identity = %#v, present %t, error %v", stored, present, err)
	}

	release, err := coordinator.Acquire(context.Background(), gateway.UID)
	if err != nil {
		t.Fatalf("acquire graph lock: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		result <- reconciler.deleteRoute(context.Background(), route.DeepCopy(), stored)
	}()
	waitForGraphReferences(t, coordinator, gateway.UID, 2)

	var changed gatewayv1.HTTPRoute
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), &changed); err != nil {
		t.Fatal(err)
	}
	changed.Generation = 2
	changed.Spec.Hostnames = []gatewayv1.Hostname{"new.example.test"}
	if err := kubeClient.Update(context.Background(), &changed); err != nil {
		t.Fatal(err)
	}
	release()

	if err := <-result; !errors.Is(err, errHTTPRouteChanged) {
		t.Fatalf("deleteRoute() error = %v, want %v", err, errHTTPRouteChanged)
	}
	if len(provider.deletedRoutes) != 0 {
		t.Fatalf("DeleteRoute calls = %d, want 0", len(provider.deletedRoutes))
	}
}

func waitForGraphReferences(t *testing.T, coordinator *GraphCoordinator, uid types.UID, want int) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for {
		got, exists := graphReferenceState(coordinator, uid)
		if (want == 0 && !exists) || (want > 0 && exists && got == want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("reference count did not become %d", want)
		}
		time.Sleep(time.Millisecond)
	}
}

func graphReferenceCount(coordinator *GraphCoordinator, uid types.UID) int {
	count, _ := graphReferenceState(coordinator, uid)
	return count
}

func graphReferenceState(coordinator *GraphCoordinator, uid types.UID) (int, bool) {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()

	if entry := coordinator.entries[uid]; entry != nil {
		return entry.refs, true
	}
	return 0, false
}

type blockingGraphProvider struct {
	gatewayStarted chan struct{}
	allowGateway   chan struct{}
	routeObserved  chan struct{}
	routeOnce      sync.Once
}

func newBlockingGraphProvider() *blockingGraphProvider {
	return &blockingGraphProvider{
		gatewayStarted: make(chan struct{}),
		allowGateway:   make(chan struct{}),
		routeObserved:  make(chan struct{}),
	}
}

func (p *blockingGraphProvider) EnsureGateway(context.Context, cloud.GatewaySpec) (cloud.GatewayState, error) {
	close(p.gatewayStarted)
	<-p.allowGateway
	return cloud.GatewayState{}, nil
}

func (p *blockingGraphProvider) GetGateway(context.Context, cloud.Identity) (cloud.GatewayState, bool, error) {
	return cloud.GatewayState{}, true, nil
}

func (p *blockingGraphProvider) DeleteGateway(context.Context, cloud.Identity) error {
	return nil
}

func (p *blockingGraphProvider) EnsureRoute(context.Context, cloud.RouteSpec) (cloud.RouteState, error) {
	return cloud.RouteState{}, nil
}

func (p *blockingGraphProvider) DeleteRoute(context.Context, cloud.Identity) error {
	p.routeOnce.Do(func() { close(p.routeObserved) })
	return nil
}
