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

package graph

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

func TestEnsureFloatingIPValidatesEveryAttachedAddress(t *testing.T) {
	identity := safetyTestIdentity(t)
	ownedDescription := identity.GatewayDescription(roleFloatingIP)

	tests := []struct {
		name         string
		floatingIPs  []map[string]string
		wantID       string
		wantConflict bool
	}{
		{
			name: "single managed Floating IP",
			floatingIPs: []map[string]string{
				{
					"id":                  "managed-fip",
					"description":         ownedDescription,
					"floating_network_id": "external-network",
					"floating_ip_address": "203.0.113.10",
					"port_id":             "vip-port",
				},
			},
			wantID: "managed-fip",
		},
		{
			name: "managed Floating IP followed by unmanaged Floating IP",
			floatingIPs: []map[string]string{
				{
					"id":                  "managed-fip",
					"description":         ownedDescription,
					"floating_network_id": "external-network",
					"floating_ip_address": "203.0.113.10",
					"port_id":             "vip-port",
				},
				{
					"id":                  "unmanaged-fip",
					"description":         "created by another controller",
					"floating_network_id": "external-network",
					"floating_ip_address": "203.0.113.11",
					"port_id":             "vip-port",
				},
			},
			wantConflict: true,
		},
		{
			name: "duplicate managed Floating IPs",
			floatingIPs: []map[string]string{
				{
					"id":                  "managed-fip-1",
					"description":         ownedDescription,
					"floating_network_id": "external-network",
					"floating_ip_address": "203.0.113.10",
					"port_id":             "vip-port",
				},
				{
					"id":                  "managed-fip-2",
					"description":         ownedDescription,
					"floating_network_id": "external-network",
					"floating_ip_address": "203.0.113.11",
					"port_id":             "vip-port",
				},
			},
			wantConflict: true,
		},
		{
			name: "managed Floating IP on another network",
			floatingIPs: []map[string]string{
				{
					"id":                  "wrong-network-fip",
					"description":         ownedDescription,
					"floating_network_id": "another-network",
					"floating_ip_address": "203.0.113.12",
					"port_id":             "vip-port",
				},
			},
			wantConflict: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, floatingIP := range test.floatingIPs {
				floatingIP["project_id"] = "project-a"
			}
			handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet || request.URL.Path != "/floatingips" {
					t.Errorf("unexpected Floating IP request: %s %s", request.Method, request.URL.RequestURI())
					http.Error(w, "unexpected request", http.StatusNotFound)
					return
				}
				if got := request.URL.Query().Get("port_id"); got != "vip-port" {
					t.Errorf("Floating IP request port_id = %q, want %q", got, "vip-port")
					http.Error(w, "unexpected port_id", http.StatusBadRequest)
					return
				}
				safetyWriteJSON(t, w, map[string]any{"floatingips": test.floatingIPs})
			})

			provider := NewProvider(ServiceClients{Network: safetyServiceClient(handler), ProjectID: "project-a"}, ProviderConfig{})
			floatingIP, outcome, err := provider.ensureFloatingIP(context.Background(), identity, "external-network", "vip-port")
			if test.wantConflict {
				if !errors.Is(err, cloud.ErrOwnershipConflict) {
					t.Fatalf("ensureFloatingIP() error = %v, want ownership conflict", err)
				}
				if floatingIP != nil {
					t.Fatalf("ensureFloatingIP() returned Floating IP %q on conflict", floatingIP.ID)
				}
				return
			}
			if err != nil {
				t.Fatalf("ensureFloatingIP() error = %v", err)
			}
			if floatingIP == nil || floatingIP.ID != test.wantID {
				t.Fatalf("ensureFloatingIP() Floating IP = %#v, want ID %q", floatingIP, test.wantID)
			}
			if outcome.State != cloud.OutcomeReady {
				t.Fatalf("ensureFloatingIP() outcome = %#v, want Ready", outcome)
			}
		})
	}
}

func TestEnsureNoFloatingIPValidatesBeforeDeletion(t *testing.T) {
	identity := safetyTestIdentity(t)
	managed := map[string]string{
		"id": "managed-fip", "description": identity.GatewayDescription(roleFloatingIP),
		"project_id": "project-a", "port_id": "vip-port",
	}
	tests := []struct {
		name         string
		floatingIPs  []map[string]string
		wantDeletes  int
		wantConflict bool
	}{
		{name: "managed address is removed", floatingIPs: []map[string]string{managed}, wantDeletes: 1},
		{name: "foreign address blocks every deletion", floatingIPs: []map[string]string{
			managed,
			{"id": "foreign-fip", "description": "owned elsewhere", "project_id": "project-a", "port_id": "vip-port"},
		}, wantConflict: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deletes := 0
			handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				switch {
				case request.Method == http.MethodGet && request.URL.Path == "/floatingips":
					safetyWriteJSON(t, w, map[string]any{"floatingips": test.floatingIPs})
				case request.Method == http.MethodDelete && request.URL.Path == "/floatingips/managed-fip":
					deletes++
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
					http.Error(w, "unexpected request", http.StatusNotFound)
				}
			})
			provider := NewProvider(ServiceClients{Network: safetyServiceClient(handler), ProjectID: "project-a"}, ProviderConfig{})
			outcome, err := provider.ensureNoFloatingIP(context.Background(), identity, "vip-port")
			if test.wantConflict && !errors.Is(err, cloud.ErrOwnershipConflict) {
				t.Fatalf("ensureNoFloatingIP() error = %v, want ownership conflict", err)
			}
			if !test.wantConflict && err != nil {
				t.Fatalf("ensureNoFloatingIP() error = %v", err)
			}
			if deletes != test.wantDeletes {
				t.Fatalf("Floating IP deletes = %d, want %d", deletes, test.wantDeletes)
			}
			if !test.wantConflict && test.wantDeletes > 0 && outcome.State != cloud.OutcomeProgressing {
				t.Fatalf("ensureNoFloatingIP() outcome = %#v, want Progressing", outcome)
			}
		})
	}
}

func TestBuildGatewayDeletionPlanUsesDedicatedListAPIs(t *testing.T) {
	identity := safetyTestIdentity(t)
	requests := newSafetyRequestLog()

	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(safetyGatewayGraphHandler(t, identity, "", requests)),
		ProjectID:    "project-a",
	}, ProviderConfig{})
	deletionPlan, err := provider.buildGatewayDeletionPlan(context.Background(), identity, "load-balancer-1")
	if err != nil {
		t.Fatalf("buildGatewayDeletionPlan() error = %v", err)
	}
	steps := deletionPlan.orderedSteps()
	wantDeletionSteps := []gatewayDeletionStep{
		{resource: gatewayDeletionL7Rule, resourceID: "rule-1", parentID: "policy-1"},
		{resource: gatewayDeletionL7Policy, resourceID: "policy-1"},
		{resource: gatewayDeletionMonitor, resourceID: "monitor-1", parentID: "pool-1"},
		{resource: gatewayDeletionMember, resourceID: "member-1", parentID: "pool-1"},
		{resource: gatewayDeletionPool, resourceID: "pool-1"},
		{resource: gatewayDeletionListener, resourceID: "listener-1"},
	}
	if len(steps) != len(wantDeletionSteps) {
		t.Fatalf("buildGatewayDeletionPlan() returned %d steps, want %d: %#v", len(steps), len(wantDeletionSteps), steps)
	}
	for index := range wantDeletionSteps {
		if steps[index] != wantDeletionSteps[index] {
			t.Errorf("buildGatewayDeletionPlan() step %d = %#v, want %#v", index, steps[index], wantDeletionSteps[index])
		}
	}

	for _, path := range []string{
		"/lbaas/listeners",
		"/lbaas/l7policies",
		"/lbaas/l7policies/policy-1/rules",
		"/lbaas/pools",
		"/lbaas/pools/pool-1/members",
		"/lbaas/healthmonitors",
	} {
		if !requests.saw(path) {
			t.Errorf("buildGatewayDeletionPlan() did not request %s", path)
		}
	}
	if requests.saw("/lbaas/loadbalancers/load-balancer-1/statuses") {
		t.Error("buildGatewayDeletionPlan() unexpectedly requested the status tree")
	}
	wantRequestOrder := []string{
		"/lbaas/listeners",
		"/lbaas/l7policies",
		"/lbaas/l7policies/policy-1/rules",
		"/lbaas/pools",
		"/lbaas/healthmonitors",
		"/lbaas/pools/pool-1/members",
	}
	gotRequestOrder := requests.orderedPaths()
	if len(gotRequestOrder) != len(wantRequestOrder) {
		t.Fatalf("buildGatewayDeletionPlan() request order = %v, want %v", gotRequestOrder, wantRequestOrder)
	}
	for index := range wantRequestOrder {
		if gotRequestOrder[index] != wantRequestOrder[index] {
			t.Fatalf("buildGatewayDeletionPlan() request order = %v, want %v", gotRequestOrder, wantRequestOrder)
		}
	}
}

func TestBuildGatewayDeletionPlanRejectsUnmanagedDescendants(t *testing.T) {
	for _, resource := range []string{"listener", "policy", "rule", "pool", "member", "monitor"} {
		t.Run(resource, func(t *testing.T) {
			identity := safetyTestIdentity(t)

			provider := NewProvider(ServiceClients{
				LoadBalancer: safetyServiceClient(safetyGatewayGraphHandler(t, identity, resource, newSafetyRequestLog())),
				ProjectID:    "project-a",
			}, ProviderConfig{})
			_, err := provider.buildGatewayDeletionPlan(context.Background(), identity, "load-balancer-1")
			if !errors.Is(err, cloud.ErrOwnershipConflict) {
				t.Fatalf("buildGatewayDeletionPlan() error = %v, want ownership conflict for unmanaged %s", err, resource)
			}
		})
	}
}

func TestExecuteGatewayDeletionStepIgnoresAlreadyDeletedResource(t *testing.T) {
	step := gatewayDeletionStep{resource: gatewayDeletionL7Rule, resourceID: "rule-1", parentID: "policy-1"}
	deleteRequests := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodDelete {
			deleteRequests++
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
		http.Error(w, "unexpected request", http.StatusNotFound)
	})
	provider := NewProvider(ServiceClients{LoadBalancer: safetyServiceClient(handler)}, ProviderConfig{})

	if err := provider.executeGatewayDeletionStep(context.Background(), step); err != nil {
		t.Fatalf("executeGatewayDeletionStep() error = %v", err)
	}
	if deleteRequests != 1 {
		t.Errorf("DELETE request count = %d, want 1", deleteRequests)
	}
}

func TestDeleteGatewayExecutesValidatedPlanWithoutCascade(t *testing.T) {
	identity := safetyTestIdentity(t)
	loadBalancerTags := safetyGatewayTags(t, identity, roleLoadBalancer)
	listenerTags := safetyGatewayTags(t, identity, roleListener)
	policyTags := safetyRouteTags(t, identity, rolePolicyExact)
	ruleTags := safetyRouteTags(t, identity, roleRulePath)
	poolTags := safetyRouteTags(t, identity, rolePool)
	memberTags := safetyRouteTags(t, identity, roleMember)
	monitorTags := safetyRouteTags(t, identity, roleMonitor)

	var deletedLoadBalancer bool
	deleted := make(map[string]int)
	var deletionOrder []string
	loadBalancerHandler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers":
			items := []any{}
			if !deletedLoadBalancer {
				items = append(items, map[string]any{
					"id": "load-balancer-1", "project_id": "project-a", "vip_port_id": "vip-port", "tags": loadBalancerTags,
				})
			}
			safetyWriteJSON(t, w, map[string]any{"loadbalancers": items})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers/load-balancer-1":
			if deletedLoadBalancer {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			safetyWriteJSON(t, w, map[string]any{"loadbalancer": map[string]any{
				"id": "load-balancer-1", "project_id": "project-a", "vip_port_id": "vip-port", "provisioning_status": "ACTIVE", "tags": loadBalancerTags,
			}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/listeners":
			items := []any{}
			if deleted["/lbaas/listeners/listener-1"] == 0 {
				items = append(items, map[string]any{"id": "listener-1", "tags": listenerTags})
			}
			safetyWriteJSON(t, w, map[string]any{"listeners": items})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/l7policies":
			items := []any{}
			if deleted["/lbaas/l7policies/policy-1"] == 0 {
				items = append(items, map[string]any{"id": "policy-1", "position": 1, "tags": policyTags})
			}
			safetyWriteJSON(t, w, map[string]any{"l7policies": items})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/l7policies/policy-1/rules":
			items := []any{}
			if deleted["/lbaas/l7policies/policy-1/rules/rule-1"] == 0 {
				items = append(items, map[string]any{"id": "rule-1", "tags": ruleTags})
			}
			safetyWriteJSON(t, w, map[string]any{"rules": items})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools":
			items := []any{}
			if deleted["/lbaas/pools/pool-1"] == 0 {
				items = append(items, map[string]any{"id": "pool-1", "tags": poolTags})
			}
			safetyWriteJSON(t, w, map[string]any{"pools": items})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools/pool-1/members":
			items := []any{}
			if deleted["/lbaas/pools/pool-1/members/member-1"] == 0 {
				items = append(items, map[string]any{"id": "member-1", "address": "10.0.0.10", "protocol_port": 30080, "tags": memberTags})
			}
			safetyWriteJSON(t, w, map[string]any{"members": items})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/healthmonitors":
			items := []any{}
			if deleted["/lbaas/healthmonitors/monitor-1"] == 0 {
				items = append(items, map[string]any{"id": "monitor-1", "tags": monitorTags})
			}
			safetyWriteJSON(t, w, map[string]any{"healthmonitors": items})
		case request.Method == http.MethodDelete:
			deleted[request.URL.Path]++
			deletionOrder = append(deletionOrder, request.URL.Path)
			if request.URL.Path == "/lbaas/loadbalancers/load-balancer-1" {
				if got := request.URL.Query().Get("cascade"); got != "" {
					t.Errorf("load balancer delete cascade = %q, want omitted", got)
					http.Error(w, "cascade must be disabled", http.StatusBadRequest)
					return
				}
				deletedLoadBalancer = true
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected load balancer request: %s %s", request.Method, request.URL.RequestURI())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})

	floatingIPDescription := identity.GatewayDescription(roleFloatingIP)
	networkHandler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/floatingips":
			if !safetyRequireQuery(t, w, request, "project_id", "project-a") {
				return
			}
			items := []any{}
			if deleted["/floatingips/floating-ip-1"] == 0 {
				items = append(items, map[string]any{
					"id": "floating-ip-1", "project_id": "project-a", "port_id": "vip-port", "description": floatingIPDescription,
				})
			}
			safetyWriteJSON(t, w, map[string]any{"floatingips": items})
		case request.Method == http.MethodDelete && request.URL.Path == "/floatingips/floating-ip-1":
			deleted[request.URL.Path]++
			deletionOrder = append(deletionOrder, request.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected network request: %s %s", request.Method, request.URL.RequestURI())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})

	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(loadBalancerHandler),
		Network:      safetyServiceClient(networkHandler),
		ProjectID:    "project-a",
	}, ProviderConfig{})
	ready := false
	for attempt := 0; attempt < 10; attempt++ {
		outcome, err := provider.DeleteGateway(context.Background(), identity.Value())
		if err != nil {
			t.Fatalf("DeleteGateway() attempt %d error = %v", attempt+1, err)
		}
		if outcome.State == cloud.OutcomeReady {
			ready = true
			break
		}
		if outcome.State != cloud.OutcomeProgressing {
			t.Fatalf("DeleteGateway() attempt %d outcome = %#v", attempt+1, outcome)
		}
	}
	if !ready {
		t.Fatal("DeleteGateway() did not converge")
	}

	for _, path := range []string{
		"/lbaas/l7policies/policy-1/rules/rule-1",
		"/lbaas/l7policies/policy-1",
		"/lbaas/listeners/listener-1",
		"/lbaas/healthmonitors/monitor-1",
		"/lbaas/pools/pool-1/members/member-1",
		"/lbaas/pools/pool-1",
		"/floatingips/floating-ip-1",
		"/lbaas/loadbalancers/load-balancer-1",
	} {
		if deleted[path] != 1 {
			t.Errorf("DELETE %s count = %d, want 1", path, deleted[path])
		}
	}
	wantDeletionOrder := []string{
		"/lbaas/l7policies/policy-1/rules/rule-1",
		"/lbaas/l7policies/policy-1",
		"/lbaas/healthmonitors/monitor-1",
		"/lbaas/pools/pool-1/members/member-1",
		"/lbaas/pools/pool-1",
		"/lbaas/listeners/listener-1",
		"/floatingips/floating-ip-1",
		"/lbaas/loadbalancers/load-balancer-1",
	}
	if len(deletionOrder) != len(wantDeletionOrder) {
		t.Fatalf("Gateway deletion order = %v, want %v", deletionOrder, wantDeletionOrder)
	}
	for index := range wantDeletionOrder {
		if deletionOrder[index] != wantDeletionOrder[index] {
			t.Errorf("Gateway deletion order[%d] = %q, want %q", index, deletionOrder[index], wantDeletionOrder[index])
		}
	}
}

func TestFindGatewayLoadBalancerScopesAuthenticatedProject(t *testing.T) {
	identity := safetyTestIdentity(t)
	tags := safetyGatewayTags(t, identity, roleLoadBalancer)
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/lbaas/loadbalancers" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		if got := request.URL.Query().Get("project_id"); got != "project-a" {
			t.Errorf("project_id query = %q, want project-a", got)
			http.Error(w, "unexpected project", http.StatusBadRequest)
			return
		}
		safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{map[string]any{
			"id": "load-balancer-1", "project_id": "project-a", "tags": tags,
		}}})
	})
	provider := NewProvider(ServiceClients{LoadBalancer: safetyServiceClient(handler), ProjectID: "project-a"}, ProviderConfig{})
	loadBalancer, err := provider.findGatewayLoadBalancer(context.Background(), identity)
	if err != nil {
		t.Fatalf("findGatewayLoadBalancer() error = %v", err)
	}
	if loadBalancer == nil || loadBalancer.ID != "load-balancer-1" {
		t.Fatalf("findGatewayLoadBalancer() = %#v", loadBalancer)
	}
}

func TestObserveRoutePoliciesRejectsForeignPolicyOrRule(t *testing.T) {
	identity := safetyTestIdentity(t)
	managedPolicyTags := safetyRouteTags(t, identity, rolePolicyExact)
	tests := []struct {
		name       string
		policyTags []string
		ruleTags   []string
	}{
		{name: "foreign policy", policyTags: []string{"owned-by=someone-else"}},
		{name: "foreign rule", policyTags: managedPolicyTags, ruleTags: []string{"owned-by=someone-else"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/lbaas/l7policies":
					if !safetyRequireQuery(t, w, request, "listener_id", "listener-1") {
						return
					}
					safetyWriteJSON(t, w, map[string]any{"l7policies": []any{map[string]any{"id": "policy-1", "tags": test.policyTags}}})
				case "/lbaas/l7policies/policy-1/rules":
					safetyWriteJSON(t, w, map[string]any{"rules": []any{map[string]any{"id": "rule-1", "tags": test.ruleTags}}})
				default:
					t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
					http.Error(w, "unexpected request", http.StatusNotFound)
				}
			})
			provider := NewProvider(ServiceClients{LoadBalancer: safetyServiceClient(handler), ProjectID: "project-a"}, ProviderConfig{})
			_, err := provider.observeRoutePolicies(context.Background(), identity, "listener-1")
			if !errors.Is(err, cloud.ErrOwnershipConflict) {
				t.Fatalf("observeRoutePolicies() error = %v, want ownership conflict", err)
			}
		})
	}
}

type safetyRequestLog struct {
	mutex    sync.Mutex
	paths    map[string]int
	sequence []string
}

func newSafetyRequestLog() *safetyRequestLog {
	return &safetyRequestLog{paths: make(map[string]int)}
}

func (log *safetyRequestLog) record(path string) {
	log.mutex.Lock()
	defer log.mutex.Unlock()
	log.paths[path]++
	log.sequence = append(log.sequence, path)
}

func (log *safetyRequestLog) saw(path string) bool {
	log.mutex.Lock()
	defer log.mutex.Unlock()
	return log.paths[path] > 0
}

func (log *safetyRequestLog) orderedPaths() []string {
	log.mutex.Lock()
	defer log.mutex.Unlock()
	return append([]string(nil), log.sequence...)
}

func safetyGatewayGraphHandler(t *testing.T, identity Identity, unmanagedResource string, requests *safetyRequestLog) http.Handler {
	t.Helper()
	tags := map[string][]string{
		"listener": safetyGatewayTags(t, identity, roleListener),
		"policy":   safetyRouteTags(t, identity, rolePolicyExact),
		"rule":     safetyRouteTags(t, identity, roleRulePath),
		"pool":     safetyRouteTags(t, identity, rolePool),
		"member":   safetyRouteTags(t, identity, roleMember),
		"monitor":  safetyRouteTags(t, identity, roleMonitor),
	}
	if unmanagedResource != "" {
		tags[unmanagedResource] = []string{"owned-by=someone-else"}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request.URL.Path)
		if request.Method != http.MethodGet {
			t.Errorf("unexpected graph request method: %s %s", request.Method, request.URL.RequestURI())
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}

		switch request.URL.Path {
		case "/lbaas/listeners":
			if !safetyRequireQuery(t, w, request, "loadbalancer_id", "load-balancer-1") {
				return
			}
			if !safetyRequireQuery(t, w, request, "project_id", "project-a") {
				return
			}
			safetyWriteJSON(t, w, map[string]any{"listeners": []any{
				map[string]any{
					"id":            "listener-1",
					"project_id":    "project-a",
					"loadbalancers": []any{map[string]any{"id": "load-balancer-1"}},
					"tags":          tags["listener"],
					"l7policies": []any{
						map[string]any{"id": "nested-policy-without-tags"},
					},
				},
			}})
		case "/lbaas/l7policies":
			if !safetyRequireQuery(t, w, request, "listener_id", "listener-1") {
				return
			}
			if !safetyRequireQuery(t, w, request, "project_id", "project-a") {
				return
			}
			safetyWriteJSON(t, w, map[string]any{"l7policies": []any{
				map[string]any{
					"id":          "policy-1",
					"project_id":  "project-a",
					"listener_id": "listener-1",
					"tags":        tags["policy"],
					"rules": []any{
						map[string]any{"id": "nested-rule-without-tags"},
					},
				},
			}})
		case "/lbaas/l7policies/policy-1/rules":
			if !safetyRequireQuery(t, w, request, "project_id", "project-a") {
				return
			}
			safetyWriteJSON(t, w, map[string]any{"rules": []any{
				map[string]any{"id": "rule-1", "project_id": "project-a", "tags": tags["rule"]},
			}})
		case "/lbaas/pools":
			if !safetyRequireQuery(t, w, request, "loadbalancer_id", "load-balancer-1") {
				return
			}
			if !safetyRequireQuery(t, w, request, "project_id", "project-a") {
				return
			}
			safetyWriteJSON(t, w, map[string]any{"pools": []any{
				map[string]any{
					"id":            "pool-1",
					"project_id":    "project-a",
					"loadbalancers": []any{map[string]any{"id": "load-balancer-1"}},
					"tags":          tags["pool"],
					"members": []any{
						map[string]any{"id": "nested-member-without-tags"},
					},
					"healthmonitor": map[string]any{"id": "nested-monitor-without-tags"},
				},
			}})
		case "/lbaas/pools/pool-1/members":
			if !safetyRequireQuery(t, w, request, "project_id", "project-a") {
				return
			}
			safetyWriteJSON(t, w, map[string]any{"members": []any{
				map[string]any{"id": "member-1", "project_id": "project-a", "pool_id": "pool-1", "tags": tags["member"]},
			}})
		case "/lbaas/healthmonitors":
			if !safetyRequireQuery(t, w, request, "pool_id", "pool-1") {
				return
			}
			if !safetyRequireQuery(t, w, request, "project_id", "project-a") {
				return
			}
			safetyWriteJSON(t, w, map[string]any{"healthmonitors": []any{
				map[string]any{
					"id": "monitor-1", "project_id": "project-a",
					"pools": []any{map[string]any{"id": "pool-1"}}, "tags": tags["monitor"],
				},
			}})
		default:
			t.Errorf("unexpected graph request: %s", request.URL.RequestURI())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})
}

func safetyTestIdentity(t *testing.T) Identity {
	t.Helper()
	identity, err := NewIdentity(cloud.Identity{
		OpenStackProjectID: "project-a",
		ClusterID:          "test-cluster",
		Controller:         "example.com/openstack-gateway-controller",
		GatewayNamespace:   "gateway-system",
		GatewayName:        "example-gateway",
		GatewayUID:         "gateway-uid",
		RouteNamespace:     "application",
		RouteName:          "example-route",
		RouteUID:           "route-uid",
	})
	if err != nil {
		t.Fatalf("NewIdentity() error = %v", err)
	}
	return identity
}

func safetyGatewayTags(t *testing.T, identity Identity, role string) []string {
	t.Helper()
	tags, err := identity.GatewayTags(role)
	if err != nil {
		t.Fatalf("GatewayTags(%q) error = %v", role, err)
	}
	return tags
}

func safetyRouteTags(t *testing.T, identity Identity, role string) []string {
	t.Helper()
	tags, err := identity.RouteTags(role)
	if err != nil {
		t.Fatalf("RouteTags(%q) error = %v", role, err)
	}
	return tags
}

type safetyRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip safetyRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func safetyServiceClient(handler http.Handler) *gophercloud.ServiceClient {
	providerClient := &gophercloud.ProviderClient{TokenID: "test-token"}
	providerClient.HTTPClient.Transport = safetyRoundTripper(func(request *http.Request) (*http.Response, error) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		result := response.Result()
		result.Request = request
		return result, nil
	})
	return &gophercloud.ServiceClient{
		ProviderClient: providerClient,
		Endpoint:       "http://openstack.test/",
	}
}

func safetyRequireQuery(t *testing.T, w http.ResponseWriter, request *http.Request, key, want string) bool {
	t.Helper()
	if got := request.URL.Query().Get(key); got != want {
		t.Errorf("%s query %s = %q, want %q", request.URL.Path, key, got, want)
		http.Error(w, "unexpected query", http.StatusBadRequest)
		return false
	}
	return true
}

func safetyWriteJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode test response: %v", err)
	}
}
