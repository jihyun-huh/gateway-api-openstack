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
	"encoding/json"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/audit"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

func TestCollectOwnershipSnapshot(t *testing.T) {
	cfg := testConfig()
	now := metav1.Now()
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "zeta",
			Name:              "edge",
			UID:               types.UID("gateway-uid"),
			DeletionTimestamp: &now,
			Finalizers:        []string{cfg.gatewayFinalizer()},
			Annotations:       gatewayBindingAnnotations(cfg, "80"),
		},
		Spec: gatewayv1.GatewaySpec{GatewayClassName: "handed-off-class"},
	}
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "alpha",
			Name:      "app",
			UID:       types.UID("route-uid"),
		},
	}
	(&HTTPRouteReconciler{Config: cfg}).applyRouteBinding(route, gateway)
	unboundGateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "unbound", UID: "unbound-gateway"}}
	unboundRoute := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "unbound", UID: "unbound-route"}}

	reader := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(route, unboundRoute, gateway, unboundGateway).
		Build()
	snapshot, err := CollectOwnershipSnapshot(context.Background(), reader, cfg)
	if err != nil {
		t.Fatalf("CollectOwnershipSnapshot() error = %v", err)
	}
	if len(snapshot.Issues) != 0 {
		t.Fatalf("CollectOwnershipSnapshot() issues = %#v, want none", snapshot.Issues)
	}
	if len(snapshot.Records) != 2 {
		t.Fatalf("CollectOwnershipSnapshot() records = %#v, want two", snapshot.Records)
	}

	wantGatewayIdentity := cloud.Identity{
		OpenStackProjectID: cfg.OpenStackProjectID,
		ClusterID:          cfg.ClusterID,
		Controller:         string(cfg.ControllerName),
		ControllerVersion:  cfg.ControllerVersion,
		GatewayNamespace:   gateway.Namespace,
		GatewayName:        gateway.Name,
		GatewayUID:         string(gateway.UID),
	}
	if snapshot.Records[0].Identity != wantGatewayIdentity {
		t.Fatalf("first record identity = %#v, want Gateway identity %#v", snapshot.Records[0].Identity, wantGatewayIdentity)
	}
	if got := snapshot.Records[0].Objects; len(got) != 1 || got[0].Kind != "Gateway" || got[0].Name != gateway.Name {
		t.Fatalf("first record objects = %#v, want Gateway", got)
	}
	wantRouteIdentity := wantGatewayIdentity
	wantRouteIdentity.RouteNamespace = route.Namespace
	wantRouteIdentity.RouteName = route.Name
	wantRouteIdentity.RouteUID = string(route.UID)
	if snapshot.Records[1].Identity != wantRouteIdentity {
		t.Fatalf("second record identity = %#v, want HTTPRoute identity %#v", snapshot.Records[1].Identity, wantRouteIdentity)
	}
	if got := snapshot.Records[1].Objects; len(got) != 1 || got[0].Kind != "HTTPRoute" || got[0].Name != route.Name {
		t.Fatalf("second record objects = %#v, want HTTPRoute", got)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	for _, privateValue := range []string{cfg.OpenStackProjectID, cfg.ClusterID, cfg.ControllerVersion} {
		if strings.Contains(string(encoded), privateValue) {
			t.Fatalf("JSON snapshot exposes private identity value %q: %s", privateValue, encoded)
		}
	}
}

func TestCollectOwnershipSnapshotReportsInvalidBindings(t *testing.T) {
	cfg := testConfig()
	partialGateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{
		Namespace:  "default",
		Name:       "partial-gateway",
		UID:        "partial-gateway-uid",
		Finalizers: []string{cfg.gatewayFinalizer()},
	}}
	wrongClusterGateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{
		Namespace:   "default",
		Name:        "wrong-cluster-gateway",
		UID:         "wrong-cluster-gateway-uid",
		Annotations: gatewayBindingAnnotations(cfg, "80"),
	}}
	wrongClusterGateway.Annotations[cfg.gatewayClusterIDAnnotation()] = "another-cluster"

	partialRoute := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Namespace:  "default",
		Name:       "partial-route",
		UID:        "partial-route-uid",
		Finalizers: []string{cfg.routeFinalizer()},
	}}
	partialRoute.Annotations = map[string]string{cfg.routeGatewayNameAnnotation(): "edge"}
	wrongProjectRoute := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default",
		Name:      "wrong-project-route",
		UID:       "wrong-project-route-uid",
	}}
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-uid"}}
	(&HTTPRouteReconciler{Config: cfg}).applyRouteBinding(wrongProjectRoute, gateway)
	wrongProjectRoute.Annotations[cfg.routeProjectIDAnnotation()] = "another-project"
	unbound := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "unbound", UID: "unbound-uid"}}

	reader := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(wrongProjectRoute, partialGateway, unbound, partialRoute, wrongClusterGateway).
		Build()
	snapshot, err := CollectOwnershipSnapshot(context.Background(), reader, cfg)
	if err != nil {
		t.Fatalf("CollectOwnershipSnapshot() error = %v", err)
	}
	if len(snapshot.Records) != 0 {
		t.Fatalf("CollectOwnershipSnapshot() records = %#v, want none", snapshot.Records)
	}
	wantNames := []string{"partial-gateway", "wrong-cluster-gateway", "partial-route", "wrong-project-route"}
	if len(snapshot.Issues) != len(wantNames) {
		t.Fatalf("CollectOwnershipSnapshot() issues = %#v, want %d", snapshot.Issues, len(wantNames))
	}
	for index, issue := range snapshot.Issues {
		if issue.Object.Name != wantNames[index] {
			t.Fatalf("issue %d object = %q, want %q", index, issue.Object.Name, wantNames[index])
		}
		if issue.Reason != invalidBindingReason || issue.Message != invalidBindingMessage {
			t.Fatalf("issue %d = %#v, want fixed redacted diagnostic", index, issue)
		}
		for _, sensitive := range []string{"another-cluster", "another-project", cfg.OpenStackProjectID} {
			if strings.Contains(issue.Message, sensitive) {
				t.Fatalf("issue %d message exposes %q: %q", index, sensitive, issue.Message)
			}
		}
	}
}

func TestCollectOwnershipSnapshotAcceptsCompleteAnnotationsWithoutFinalizers(t *testing.T) {
	cfg := testConfig()
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{
		Namespace:   "default",
		Name:        "edge",
		UID:         "gateway-uid",
		Annotations: gatewayBindingAnnotations(cfg, "80"),
	}}
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default",
		Name:      "app",
		UID:       "route-uid",
		Annotations: map[string]string{
			cfg.routeGatewayNamespaceAnnotation(): gateway.Namespace,
			cfg.routeGatewayNameAnnotation():      gateway.Name,
			cfg.routeGatewayUIDAnnotation():       string(gateway.UID),
			cfg.routeClusterIDAnnotation():        cfg.ClusterID,
			cfg.routeProjectIDAnnotation():        cfg.OpenStackProjectID,
		},
	}}
	reader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(gateway, route).Build()

	snapshot, err := CollectOwnershipSnapshot(context.Background(), reader, cfg)
	if err != nil {
		t.Fatalf("CollectOwnershipSnapshot() error = %v", err)
	}
	if len(snapshot.Records) != 2 || len(snapshot.Issues) != 0 {
		t.Fatalf("CollectOwnershipSnapshot() = %#v, want two records and no issues", snapshot)
	}
}

func TestCollectOwnershipSnapshotDiscardsPartialList(t *testing.T) {
	cfg := testConfig()
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{
		Namespace:   "default",
		Name:        "edge",
		UID:         "gateway-uid",
		Annotations: gatewayBindingAnnotations(cfg, "80"),
	}}
	baseReader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(gateway).Build()
	reader := &ownershipAuditFailingReader{Reader: baseReader, err: errors.New("route list unavailable")}

	snapshot, err := CollectOwnershipSnapshot(context.Background(), reader, cfg)
	if err == nil || !strings.Contains(err.Error(), "list HTTPRoutes") {
		t.Fatalf("CollectOwnershipSnapshot() error = %v, want HTTPRoute list error", err)
	}
	if snapshot.Records != nil || snapshot.Issues != nil {
		t.Fatalf("CollectOwnershipSnapshot() returned partial data: %#v", snapshot)
	}
}

func TestCollectOwnershipSnapshotValidatesOnlyAuditConfiguration(t *testing.T) {
	cfg := testConfig()
	cfg.Provider = ""
	cfg.VIPSubnetID = ""
	cfg.MemberSubnetID = ""
	cfg.MemberMode = ""
	cfg.HealthPath = ""
	cfg.OpenStackResyncInterval = 0
	reader := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()

	snapshot, err := CollectOwnershipSnapshot(context.Background(), reader, cfg)
	if err != nil {
		t.Fatalf("CollectOwnershipSnapshot() error = %v, want read-only configuration to be sufficient", err)
	}
	if snapshot.Records == nil || snapshot.Issues == nil {
		t.Fatalf("CollectOwnershipSnapshot() = %#v, want non-nil empty slices", snapshot)
	}

	cfg.OpenStackProjectID = ""
	if _, err := CollectOwnershipSnapshot(context.Background(), reader, cfg); err == nil {
		t.Fatal("CollectOwnershipSnapshot() error = nil, want invalid scope")
	}
}

type ownershipAuditFailingReader struct {
	client.Reader
	err error
}

func (r *ownershipAuditFailingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*gatewayv1.HTTPRouteList); ok {
		return r.err
	}
	return r.Reader.List(ctx, list, opts...)
}

func TestSortOwnershipSnapshot(t *testing.T) {
	snapshot := audit.KubernetesSnapshot{
		Records: []audit.OwnershipRecord{
			{Objects: []audit.ObjectReference{{Kind: "HTTPRoute", Namespace: "z", Name: "route", UID: "2"}}},
			{Objects: []audit.ObjectReference{{Kind: "Gateway", Namespace: "a", Name: "gateway", UID: "1"}}},
		},
		Issues: []audit.KubernetesIssue{
			{Object: audit.ObjectReference{Kind: "HTTPRoute", Namespace: "z", Name: "route", UID: "2"}},
			{Object: audit.ObjectReference{Kind: "Gateway", Namespace: "a", Name: "gateway", UID: "1"}},
		},
	}

	sortOwnershipSnapshot(&snapshot)
	if snapshot.Records[0].Objects[0].Kind != "Gateway" || snapshot.Issues[0].Object.Kind != "Gateway" {
		t.Fatalf("sortOwnershipSnapshot() = %#v, want Gateway entries first", snapshot)
	}
}
