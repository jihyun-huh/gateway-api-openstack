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

package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jihyun-huh/gateway-api-openstack/internal/audit"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack/clients"
	openstackidentity "github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack/identity"
)

func TestScanReturnsMatchedGraphUsingReadOnlyPaginatedQueries(t *testing.T) {
	identity := safetyTestIdentity(t)
	fixture := newAuditScanFixture(t, identity)
	fixture.paginate = true

	provider := fixture.provider()
	inventory, err := provider.Scan(context.Background(), auditTestScope(identity.Value()), auditTestRecords(identity.Value()))
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}

	if len(inventory.Resources) != 8 {
		t.Fatalf("Scan() returned %d resources, want 8: %#v; requests: %#v", len(inventory.Resources), inventory.Resources, fixture.recordedRequests())
	}
	wantParents := map[string]string{
		"floating-ip-1": "load-balancer-1",
		"listener-1":    "load-balancer-1",
		"pool-1":        "load-balancer-1",
		"member-1":      "pool-1",
		"monitor-1":     "pool-1",
		"policy-1":      "listener-1",
		"rule-1":        "policy-1",
	}
	for _, finding := range inventory.Resources {
		if finding.Disposition != audit.DispositionMatched || finding.Reason != auditReasonExactBinding {
			t.Errorf("resource %s disposition = %q, reason = %q, want matched ExactBinding", finding.ID, finding.Disposition, finding.Reason)
		}
		if want := wantParents[finding.ID]; finding.ParentID != want {
			t.Errorf("resource %s parent = %q, want %q", finding.ID, finding.ParentID, want)
		}
		if len(finding.Objects) != 1 {
			t.Errorf("resource %s objects = %#v, want one object", finding.ID, finding.Objects)
		}
	}
	if len(inventory.Limitations) == 0 {
		t.Error("Scan() returned no documented limitations")
	}
	for _, limitation := range inventory.Limitations {
		if limitation == "" {
			t.Errorf("Scan() limitations contain an empty entry: %#v", inventory.Limitations)
		}
	}

	requests := fixture.recordedRequests()
	if len(requests) == 0 {
		t.Fatal("Scan() made no OpenStack requests")
	}
	for _, request := range requests {
		if request.method != http.MethodGet {
			t.Errorf("Scan() sent mutating request %s %s", request.method, request.path)
		}
		if got := request.query.Get("project_id"); got != identity.Value().OpenStackProjectID {
			t.Errorf("%s query project_id = %q, want %q", request.path, got, identity.Value().OpenStackProjectID)
		}
	}
	if fixture.mutatingRequests != 0 {
		t.Fatalf("Scan() sent %d mutating requests, want 0", fixture.mutatingRequests)
	}

	wantScopeTags := openstackidentity.ScopeTags(identity.Value().ClusterID, identity.Value().Controller)
	slices.Sort(wantScopeTags)
	for _, path := range []string{"/lbaas/loadbalancers", "/lbaas/listeners", "/lbaas/pools", "/lbaas/healthmonitors"} {
		request, ok := fixture.findRequest(path, func(query url.Values) bool {
			return query.Get("tags") != "" && query.Get("page") == ""
		})
		if !ok {
			t.Errorf("Scan() did not send a scope tag query to %s", path)
			continue
		}
		gotTags := append([]string(nil), request.query["tags"]...)
		slices.Sort(gotTags)
		if !slices.Equal(gotTags, wantScopeTags) {
			t.Errorf("%s scope tags = %#v, want %#v", path, gotTags, wantScopeTags)
		}
	}
	for path, key := range map[string]string{
		"/lbaas/listeners":      "loadbalancer_id",
		"/lbaas/pools":          "loadbalancer_id",
		"/lbaas/healthmonitors": "pool_id",
	} {
		if _, ok := fixture.findRequest(path, func(query url.Values) bool {
			return query.Get(key) != "" && query.Get("page") == ""
		}); !ok {
			t.Errorf("Scan() did not send a parent-scoped query to %s", path)
		}
	}

	wantPagedPaths := []string{
		"/floatingips",
		"/lbaas/healthmonitors",
		"/lbaas/l7policies",
		"/lbaas/l7policies/policy-1/rules",
		"/lbaas/listeners",
		"/lbaas/loadbalancers",
		"/lbaas/pools",
		"/lbaas/pools/pool-1/members",
	}
	for _, path := range wantPagedPaths {
		if _, ok := fixture.findRequest(path, func(query url.Values) bool { return query.Get("page") == "2" }); !ok {
			t.Errorf("Scan() did not follow the next page for %s", path)
		}
	}

	encoded, err := json.Marshal(inventory)
	if err != nil {
		t.Fatalf("json.Marshal(inventory) error = %v", err)
	}
	for _, sensitive := range []string{
		"raw-private-tag",
		"raw-private-description",
		"198.51.100.44",
		"203.0.113.44",
		identity.Value().OpenStackProjectID,
		strings.Join(safetyGatewayTags(t, identity, roleLoadBalancer), ","),
		identity.GatewayDescription(roleFloatingIP),
	} {
		if strings.Contains(string(encoded), sensitive) {
			t.Errorf("serialized inventory contains sensitive provider data %q: %s", sensitive, encoded)
		}
	}
}

func TestScanClassifiesOwnershipFindings(t *testing.T) {
	current := safetyTestIdentity(t)
	currentGateway := auditGatewayIdentity(current.Value())

	tests := []struct {
		name             string
		configure        func(*testing.T, *auditScanFixture)
		records          []audit.OwnershipRecord
		wantDisposition  audit.Disposition
		wantReason       string
		wantFindingCount int
	}{
		{
			name: "orphan candidate",
			configure: func(t *testing.T, fixture *auditScanFixture) {
				fixture.keepOnlyLoadBalancers()
			},
			wantDisposition:  audit.DispositionOrphanCandidate,
			wantReason:       auditReasonNoBinding,
			wantFindingCount: 1,
		},
		{
			name: "stale Gateway UID",
			configure: func(t *testing.T, fixture *auditScanFixture) {
				fixture.keepOnlyLoadBalancers()
				stale := current.Value()
				stale.GatewayUID = "deleted-gateway-uid"
				fixture.loadBalancers[0]["tags"] = auditGatewayTagsFor(t, stale, roleLoadBalancer)
			},
			records: []audit.OwnershipRecord{{
				Identity: currentGateway,
				Objects:  []audit.ObjectReference{auditGatewayReference(currentGateway)},
			}},
			wantDisposition:  audit.DispositionStaleUID,
			wantReason:       auditReasonStaleUID,
			wantFindingCount: 1,
		},
		{
			name: "wrong load balancer provider",
			configure: func(t *testing.T, fixture *auditScanFixture) {
				fixture.keepOnlyLoadBalancers()
				fixture.loadBalancers[0]["provider"] = "ovn"
			},
			records: []audit.OwnershipRecord{{
				Identity: currentGateway,
				Objects:  []audit.ObjectReference{auditGatewayReference(currentGateway)},
			}},
			wantDisposition:  audit.DispositionOwnershipConflict,
			wantReason:       auditReasonInvalidIdentity,
			wantFindingCount: 1,
		},
		{
			name: "resource reports another project",
			configure: func(t *testing.T, fixture *auditScanFixture) {
				fixture.keepOnlyLoadBalancers()
				fixture.loadBalancers[0]["project_id"] = "foreign-project"
			},
			wantDisposition:  audit.DispositionOwnershipConflict,
			wantReason:       auditReasonProjectMismatch,
			wantFindingCount: 1,
		},
		{
			name: "descendant lost scope identity",
			configure: func(t *testing.T, fixture *auditScanFixture) {
				fixture.pools = nil
				fixture.members = nil
				fixture.monitors = nil
				fixture.policies = nil
				fixture.rules = nil
				fixture.floatingIPs = nil
				fixture.listeners[0]["tags"] = []string{"owned-by=another-controller"}
			},
			records: []audit.OwnershipRecord{{
				Identity: currentGateway,
				Objects:  []audit.ObjectReference{auditGatewayReference(currentGateway)},
			}},
			wantDisposition:  audit.DispositionOwnershipConflict,
			wantReason:       auditReasonInvalidIdentity,
			wantFindingCount: 2,
		},
		{
			name: "duplicate logical identity",
			configure: func(t *testing.T, fixture *auditScanFixture) {
				fixture.keepOnlyLoadBalancers()
				duplicate := auditCloneMap(fixture.loadBalancers[0])
				duplicate["id"] = "load-balancer-2"
				duplicate["vip_port_id"] = "vip-port-2"
				fixture.loadBalancers = append(fixture.loadBalancers, duplicate)
			},
			wantDisposition:  audit.DispositionOwnershipConflict,
			wantReason:       auditReasonDuplicateIdentity,
			wantFindingCount: 2,
		},
		{
			name: "detached Floating IP",
			configure: func(t *testing.T, fixture *auditScanFixture) {
				fixture.loadBalancers = nil
				fixture.listeners = nil
				fixture.pools = nil
				fixture.members = nil
				fixture.monitors = nil
				fixture.policies = nil
				fixture.rules = nil
				fixture.floatingIPs[0]["port_id"] = ""
			},
			records: []audit.OwnershipRecord{{
				Identity: currentGateway,
				Objects:  []audit.ObjectReference{auditGatewayReference(currentGateway)},
			}},
			wantDisposition:  audit.DispositionUnresolved,
			wantReason:       auditReasonDetachedFloatingIP,
			wantFindingCount: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAuditScanFixture(t, current)
			test.configure(t, fixture)
			inventory, err := fixture.provider().Scan(context.Background(), auditTestScope(current.Value()), test.records)
			if err != nil {
				t.Fatalf("Scan() error = %v", err)
			}
			if len(inventory.Resources) != test.wantFindingCount {
				t.Fatalf("Scan() returned %d findings, want %d: %#v", len(inventory.Resources), test.wantFindingCount, inventory.Resources)
			}
			matched := 0
			for _, finding := range inventory.Resources {
				if finding.Disposition == test.wantDisposition && finding.Reason == test.wantReason {
					matched++
				}
			}
			wantMatches := 1
			if test.wantReason == auditReasonDuplicateIdentity {
				wantMatches = 2
			}
			if matched != wantMatches {
				t.Errorf("Scan() returned %d findings with %s/%s, want %d: %#v", matched, test.wantDisposition, test.wantReason, wantMatches, inventory.Resources)
			}
			if fixture.mutatingRequests != 0 {
				t.Errorf("Scan() sent %d mutating requests, want 0", fixture.mutatingRequests)
			}
		})
	}
}

func TestScanSortsFindingsDeterministically(t *testing.T) {
	base := safetyTestIdentity(t)
	identityA := auditIdentityForGateway(t, base.Value(), "gateway-a", "gateway-a-uid")
	identityB := auditIdentityForGateway(t, base.Value(), "gateway-b", "gateway-b-uid")
	fixture := newAuditScanFixture(t, base)
	fixture.keepOnlyLoadBalancers()
	fixture.loadBalancers = []map[string]any{
		auditLoadBalancer(t, "load-balancer-b", "vip-port-b", identityB),
		auditLoadBalancer(t, "load-balancer-a", "vip-port-a", identityA),
	}

	first, err := fixture.provider().Scan(context.Background(), auditTestScope(base.Value()), nil)
	if err != nil {
		t.Fatalf("first Scan() error = %v", err)
	}
	if got := auditFindingIDs(first.Resources); !slices.Equal(got, []string{"load-balancer-a", "load-balancer-b"}) {
		t.Fatalf("first Scan() order = %#v, want sorted IDs", got)
	}

	slices.Reverse(fixture.loadBalancers)
	second, err := fixture.provider().Scan(context.Background(), auditTestScope(base.Value()), nil)
	if err != nil {
		t.Fatalf("second Scan() error = %v", err)
	}
	if !reflect.DeepEqual(second, first) {
		t.Errorf("Scan() result changed with server response order\nfirst:  %#v\nsecond: %#v", first, second)
	}
}

func TestScanOmitsUnattributedDetachedFloatingIP(t *testing.T) {
	current := safetyTestIdentity(t)
	foreignValue := current.Value()
	foreignValue.ClusterID = "another-cluster"
	foreignValue.Controller = "other.example/gateway-controller"
	foreignValue.GatewayName = "other-gateway"
	foreignValue.GatewayUID = "other-gateway-uid"
	foreignValue.RouteNamespace = ""
	foreignValue.RouteName = ""
	foreignValue.RouteUID = ""
	foreign, err := openstackidentity.NewIdentity(foreignValue)
	if err != nil {
		t.Fatalf("openstackidentity.NewIdentity() error = %v", err)
	}
	fixture := newAuditScanFixture(t, current)
	fixture.loadBalancers = nil
	fixture.listeners = nil
	fixture.pools = nil
	fixture.members = nil
	fixture.monitors = nil
	fixture.policies = nil
	fixture.rules = nil
	fixture.floatingIPs = []map[string]any{{
		"id": "floating-ip-foreign", "project_id": current.Value().OpenStackProjectID,
		"port_id": "", "description": foreign.GatewayDescription(roleFloatingIP),
	}}

	inventory, err := fixture.provider().Scan(
		context.Background(),
		auditTestScope(current.Value()),
		[]audit.OwnershipRecord{{
			Identity: auditGatewayIdentity(current.Value()),
			Objects:  []audit.ObjectReference{auditGatewayReference(current.Value())},
		}},
	)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(inventory.Resources) != 0 {
		t.Fatalf("Scan() resources = %#v, want the unattributed Floating IP omitted", inventory.Resources)
	}
}

func TestScanRejectsProjectMismatchBeforeRequest(t *testing.T) {
	requests := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	})
	provider := NewScanner(clients.ServiceClients{
		LoadBalancer: safetyServiceClient(handler),
		Network:      safetyServiceClient(handler),
		ProjectID:    "project-a",
	}, 0)

	_, err := provider.Scan(context.Background(), audit.Scope{
		ClusterID:          "test-cluster",
		ControllerName:     "example.com/openstack-gateway-controller",
		OpenStackProjectID: "project-b",
	}, nil)
	if !errors.Is(err, cloud.ErrOwnershipConflict) {
		t.Fatalf("Scan() error = %v, want ownership conflict", err)
	}
	for _, projectID := range []string{"project-a", "project-b"} {
		if strings.Contains(err.Error(), projectID) {
			t.Errorf("Scan() error exposes OpenStack project ID %q: %v", projectID, err)
		}
	}
	if requests != 0 {
		t.Errorf("Scan() made %d requests before rejecting the project mismatch", requests)
	}
}

type auditRecordedRequest struct {
	method string
	path   string
	query  url.Values
}

type auditScanFixture struct {
	t                *testing.T
	loadBalancers    []map[string]any
	listeners        []map[string]any
	pools            []map[string]any
	members          []map[string]any
	monitors         []map[string]any
	policies         []map[string]any
	rules            []map[string]any
	floatingIPs      []map[string]any
	paginate         bool
	mutex            sync.Mutex
	requests         []auditRecordedRequest
	mutatingRequests int
}

func newAuditScanFixture(t *testing.T, identity openstackidentity.Identity) *auditScanFixture {
	t.Helper()
	loadBalancerTags := append(safetyGatewayTags(t, identity, roleLoadBalancer), "raw-private-tag=customer-a")
	listenerTags := safetyGatewayTags(t, identity, roleListener)
	poolTags := safetyRouteTags(t, identity, rolePool)
	memberTags := safetyRouteTags(t, identity, roleMember)
	monitorTags := safetyRouteTags(t, identity, roleMonitor)
	policyTags := safetyRouteTags(t, identity, rolePolicyExact)
	ruleTags := safetyRouteTags(t, identity, roleRulePath)

	return &auditScanFixture{
		t: t,
		loadBalancers: []map[string]any{{
			"id": "load-balancer-1", "project_id": identity.Value().OpenStackProjectID,
			"provider": "amphora", "provisioning_status": "ACTIVE",
			"vip_port_id": "vip-port-1", "vip_address": "198.51.100.44", "tags": loadBalancerTags,
		}},
		listeners: []map[string]any{{
			"id": "listener-1", "project_id": identity.Value().OpenStackProjectID,
			"provisioning_status": "ACTIVE", "tags": listenerTags,
			"loadbalancers": []any{map[string]any{"id": "load-balancer-1"}},
		}},
		pools: []map[string]any{{
			"id": "pool-1", "project_id": identity.Value().OpenStackProjectID,
			"provisioning_status": "ACTIVE", "tags": poolTags,
			"loadbalancers": []any{map[string]any{"id": "load-balancer-1"}},
		}},
		members: []map[string]any{{
			"id": "member-1", "project_id": identity.Value().OpenStackProjectID,
			"provisioning_status": "ACTIVE", "pool_id": "pool-1", "tags": memberTags,
			"address": "192.0.2.10",
		}},
		monitors: []map[string]any{{
			"id": "monitor-1", "project_id": identity.Value().OpenStackProjectID,
			"provisioning_status": "ACTIVE", "tags": monitorTags,
			"pools": []any{map[string]any{"id": "pool-1"}},
		}},
		policies: []map[string]any{{
			"id": "policy-1", "project_id": identity.Value().OpenStackProjectID,
			"provisioning_status": "ACTIVE", "listener_id": "listener-1",
			"redirect_pool_id": "pool-1", "tags": policyTags,
		}},
		rules: []map[string]any{{
			"id": "rule-1", "project_id": identity.Value().OpenStackProjectID,
			"provisioning_status": "ACTIVE", "policy_id": "policy-1", "tags": ruleTags,
		}},
		floatingIPs: []map[string]any{{
			"id": "floating-ip-1", "project_id": identity.Value().OpenStackProjectID,
			"port_id": "vip-port-1", "floating_ip_address": "203.0.113.44",
			"description": identity.GatewayDescription(roleFloatingIP) + "; operator-note=raw-private-description",
		}},
	}
}

func (f *auditScanFixture) provider() *Scanner {
	handler := http.HandlerFunc(f.serveHTTP)
	return NewScanner(clients.ServiceClients{
		LoadBalancer: safetyServiceClient(handler),
		Network:      safetyServiceClient(handler),
		ProjectID:    "project-a",
	}, 0)
}

func (f *auditScanFixture) serveHTTP(w http.ResponseWriter, request *http.Request) {
	f.record(request)
	if request.Method != http.MethodGet {
		f.mutex.Lock()
		f.mutatingRequests++
		f.mutex.Unlock()
		http.Error(w, "audit must be read only", http.StatusMethodNotAllowed)
		return
	}

	var key string
	var items []map[string]any
	switch {
	case request.URL.Path == "/lbaas/loadbalancers":
		key, items = "loadbalancers", auditFilterTags(f.loadBalancers, request.URL.Query()["tags"])
	case request.URL.Path == "/lbaas/listeners":
		key = "listeners"
		items = auditFilterByParent(f.listeners, "loadbalancers", request.URL.Query().Get("loadbalancer_id"))
		items = auditFilterTags(items, request.URL.Query()["tags"])
	case request.URL.Path == "/lbaas/pools":
		key = "pools"
		items = auditFilterByParent(f.pools, "loadbalancers", request.URL.Query().Get("loadbalancer_id"))
		items = auditFilterTags(items, request.URL.Query()["tags"])
	case request.URL.Path == "/lbaas/l7policies":
		key, items = "l7policies", f.policies
	case strings.HasPrefix(request.URL.Path, "/lbaas/pools/") && strings.HasSuffix(request.URL.Path, "/members"):
		key = "members"
		poolID := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/lbaas/pools/"), "/members")
		items = auditFilterScalar(f.members, "pool_id", poolID)
	case request.URL.Path == "/lbaas/healthmonitors":
		key = "healthmonitors"
		items = auditFilterByParent(f.monitors, "pools", request.URL.Query().Get("pool_id"))
		items = auditFilterTags(items, request.URL.Query()["tags"])
	case strings.HasPrefix(request.URL.Path, "/lbaas/l7policies/") && strings.HasSuffix(request.URL.Path, "/rules"):
		key = "rules"
		policyID := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/lbaas/l7policies/"), "/rules")
		items = auditFilterScalar(f.rules, "policy_id", policyID)
	case request.URL.Path == "/floatingips":
		key, items = "floatingips", f.floatingIPs
	default:
		f.t.Errorf("unexpected audit request: %s %s", request.Method, request.URL.RequestURI())
		http.Error(w, "unexpected request", http.StatusNotFound)
		return
	}
	f.writeCollection(w, request, key, items)
}

func (f *auditScanFixture) writeCollection(w http.ResponseWriter, request *http.Request, key string, items []map[string]any) {
	if f.paginate && request.URL.Query().Get("page") == "" {
		query := request.URL.Query()
		query.Set("page", "2")
		next := "http://openstack.test" + request.URL.Path + "?" + query.Encode()
		safetyWriteJSON(f.t, w, map[string]any{
			key:            items,
			key + "_links": []any{map[string]any{"rel": "next", "href": next}},
		})
		return
	}
	if f.paginate {
		items = nil
	}
	safetyWriteJSON(f.t, w, map[string]any{key: items})
}

func (f *auditScanFixture) record(request *http.Request) {
	query := make(url.Values, len(request.URL.Query()))
	for key, values := range request.URL.Query() {
		query[key] = append([]string(nil), values...)
	}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.requests = append(f.requests, auditRecordedRequest{method: request.Method, path: request.URL.Path, query: query})
}

func (f *auditScanFixture) recordedRequests() []auditRecordedRequest {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return append([]auditRecordedRequest(nil), f.requests...)
}

func (f *auditScanFixture) findRequest(path string, match func(url.Values) bool) (auditRecordedRequest, bool) {
	for _, request := range f.recordedRequests() {
		if request.path == path && match(request.query) {
			return request, true
		}
	}
	return auditRecordedRequest{}, false
}

func (f *auditScanFixture) keepOnlyLoadBalancers() {
	f.listeners = nil
	f.pools = nil
	f.members = nil
	f.monitors = nil
	f.policies = nil
	f.rules = nil
	f.floatingIPs = nil
}

func auditTestScope(identity cloud.Identity) audit.Scope {
	return audit.Scope{
		ClusterID:          identity.ClusterID,
		ControllerName:     identity.Controller,
		OpenStackProjectID: identity.OpenStackProjectID,
	}
}

func auditTestRecords(identity cloud.Identity) []audit.OwnershipRecord {
	gatewayIdentity := auditGatewayIdentity(identity)
	return []audit.OwnershipRecord{
		{Identity: gatewayIdentity, Objects: []audit.ObjectReference{auditGatewayReference(gatewayIdentity)}},
		{Identity: identity, Objects: []audit.ObjectReference{auditRouteReference(identity)}},
	}
}

func auditGatewayIdentity(identity cloud.Identity) cloud.Identity {
	identity.RouteNamespace = ""
	identity.RouteName = ""
	identity.RouteUID = ""
	return identity
}

func auditGatewayReference(identity cloud.Identity) audit.ObjectReference {
	return audit.ObjectReference{
		APIVersion: "gateway.networking.k8s.io/v1", Kind: "Gateway",
		Namespace: identity.GatewayNamespace, Name: identity.GatewayName, UID: identity.GatewayUID,
	}
}

func auditRouteReference(identity cloud.Identity) audit.ObjectReference {
	return audit.ObjectReference{
		APIVersion: "gateway.networking.k8s.io/v1", Kind: "HTTPRoute",
		Namespace: identity.RouteNamespace, Name: identity.RouteName, UID: identity.RouteUID,
	}
}

func auditIdentityForGateway(t *testing.T, base cloud.Identity, name, uid string) openstackidentity.Identity {
	t.Helper()
	base.GatewayName = name
	base.GatewayUID = uid
	base.RouteNamespace = ""
	base.RouteName = ""
	base.RouteUID = ""
	identity, err := openstackidentity.NewIdentity(base)
	if err != nil {
		t.Fatalf("openstackidentity.NewIdentity() error = %v", err)
	}
	return identity
}

func auditLoadBalancer(t *testing.T, id, portID string, identity openstackidentity.Identity) map[string]any {
	t.Helper()
	return map[string]any{
		"id": id, "project_id": identity.Value().OpenStackProjectID,
		"provider": "amphora", "provisioning_status": "ACTIVE",
		"vip_port_id": portID, "tags": safetyGatewayTags(t, identity, roleLoadBalancer),
	}
}

func auditGatewayTagsFor(t *testing.T, identity cloud.Identity, role string) []string {
	t.Helper()
	providerIdentity, err := openstackidentity.NewIdentity(identity)
	if err != nil {
		t.Fatalf("openstackidentity.NewIdentity() error = %v", err)
	}
	return safetyGatewayTags(t, providerIdentity, role)
}

func auditFindingIDs(findings []audit.ResourceFinding) []string {
	ids := make([]string, 0, len(findings))
	for _, finding := range findings {
		ids = append(ids, finding.ID)
	}
	return ids
}

func auditFilterTags(items []map[string]any, query []string) []map[string]any {
	if len(query) == 0 {
		return items
	}
	wanted := make([]string, 0, len(query))
	for _, value := range query {
		wanted = append(wanted, strings.Split(value, ",")...)
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		tags, _ := item["tags"].([]string)
		if auditContainsStrings(tags, wanted) {
			result = append(result, item)
		}
	}
	return result
}

func auditContainsStrings(values, wanted []string) bool {
	for _, wantedValue := range wanted {
		if !slices.Contains(values, wantedValue) {
			return false
		}
	}
	return true
}

func auditFilterByParent(items []map[string]any, field, parentID string) []map[string]any {
	if parentID == "" {
		return items
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		parents, _ := item[field].([]any)
		for _, value := range parents {
			parent, _ := value.(map[string]any)
			if parent["id"] == parentID {
				result = append(result, item)
				break
			}
		}
	}
	return result
}

func auditFilterScalar(items []map[string]any, field, value string) []map[string]any {
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if fmt.Sprint(item[field]) == value {
			result = append(result, item)
		}
	}
	return result
}

func auditCloneMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
