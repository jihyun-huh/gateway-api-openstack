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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller/graph"
)

func TestReconcilersShareGatewayGraphCoordinator(t *testing.T) {
	config := testConfig()
	coordinator := &graph.Coordinator{}
	provider := newBlockingGraphProvider()
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "default",
			Name:        "edge",
			UID:         "gateway-1",
			Generation:  1,
			Finalizers:  []string{config.GatewayFinalizer()},
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
	routeBinder := &Reconciler{Config: config}
	routeBinder.applyRouteBinding(route, gateway)
	kubeClient := indexedFakeClientBuilder(testScheme(t), config).WithObjects(route).Build()
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), route); err != nil {
		t.Fatal(err)
	}
	routeReconciler := &Reconciler{Client: kubeClient, Provider: provider, Coordinator: coordinator, APIReader: kubeClient, Config: config}
	storedRouteIdentity, present, err := routeReconciler.storedRouteIdentity(route)
	if err != nil || !present {
		t.Fatalf("stored route identity = %#v, present %t, error %v", storedRouteIdentity, present, err)
	}
	releaseGatewayWriter, err := graph.Acquire(context.Background(), coordinator, string(gateway.UID))
	if err != nil {
		t.Fatalf("acquire Gateway graph: %v", err)
	}

	routeDone := make(chan error, 1)
	go func() {
		_, err := routeReconciler.deleteRoute(context.Background(), route.DeepCopy(), storedRouteIdentity)
		routeDone <- err
	}()
	select {
	case <-provider.routeObserved:
		t.Fatal("HTTPRoute observed the graph while Gateway mutation was active")
	case <-time.After(25 * time.Millisecond):
	}

	releaseGatewayWriter()
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
}

func TestHTTPRouteRevalidatesSnapshotAfterWaitingForGraph(t *testing.T) {
	config := testConfig()
	coordinator := &graph.Coordinator{}
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-1", Generation: 1}}
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-1"
	route.Generation = 1
	kubeClient := indexedFakeClientBuilder(testScheme(t), config).WithObjects(gateway, route).Build()
	provider := &recordingProvider{}
	reconciler := &Reconciler{Client: kubeClient, Provider: provider, Coordinator: coordinator, APIReader: kubeClient, Config: config}

	release, err := graph.Acquire(context.Background(), coordinator, string(gateway.UID))
	if err != nil {
		t.Fatalf("acquire graph lock: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := reconciler.ensureRouteGraph(context.Background(), route.DeepCopy(), gateway.DeepCopy())
		result <- err
	}()
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
	coordinator := &graph.Coordinator{}
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-1"}}
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-1"
	route.Generation = 1
	binder := &Reconciler{Config: config}
	binder.applyRouteBinding(route, gateway)
	kubeClient := indexedFakeClientBuilder(testScheme(t), config).WithObjects(route).Build()
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), route); err != nil {
		t.Fatal(err)
	}
	provider := &recordingProvider{}
	reconciler := &Reconciler{Client: kubeClient, Provider: provider, Coordinator: coordinator, APIReader: kubeClient, Config: config}
	stored, present, err := reconciler.storedRouteIdentity(route)
	if err != nil || !present {
		t.Fatalf("stored route identity = %#v, present %t, error %v", stored, present, err)
	}

	release, err := graph.Acquire(context.Background(), coordinator, string(gateway.UID))
	if err != nil {
		t.Fatalf("acquire graph lock: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := reconciler.deleteRoute(context.Background(), route.DeepCopy(), stored)
		result <- err
	}()
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

type blockingGraphProvider struct {
	routeObserved chan struct{}
	routeOnce     sync.Once
}

func newBlockingGraphProvider() *blockingGraphProvider {
	return &blockingGraphProvider{
		routeObserved: make(chan struct{}),
	}
}

func (p *blockingGraphProvider) EnsureGateway(context.Context, cloud.GatewaySpec) (cloud.GatewayResult, error) {
	return cloud.GatewayReadyResult(cloud.GatewayState{}), nil
}

func (p *blockingGraphProvider) GetGateway(context.Context, cloud.Identity) (cloud.GatewayResult, bool, error) {
	return cloud.GatewayReadyResult(cloud.GatewayState{}), true, nil
}

func (p *blockingGraphProvider) DeleteGateway(context.Context, cloud.Identity) (cloud.Outcome, error) {
	return cloud.ReadyOutcome(), nil
}

func (p *blockingGraphProvider) EnsureRoute(context.Context, cloud.RouteSpec) (cloud.RouteResult, error) {
	return cloud.RouteReadyResult(cloud.RouteState{}), nil
}

func (p *blockingGraphProvider) DeleteRoute(context.Context, cloud.Identity) (cloud.Outcome, error) {
	p.routeOnce.Do(func() { close(p.routeObserved) })
	return cloud.ReadyOutcome(), nil
}
