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
	"errors"
	"net/http"
	"testing"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

func TestDeleteGatewayRevalidatesLoadBalancerAfterGet(t *testing.T) {
	identity := safetyTestIdentity(t)
	managedTags := safetyGatewayTags(t, identity, roleLoadBalancer)
	tests := []struct {
		name               string
		getID              string
		getProjectID       string
		getTags            []string
		provisioningStatus string
	}{
		{
			name:               "project changed",
			getID:              "load-balancer-1",
			getProjectID:       "project-b",
			getTags:            managedTags,
			provisioningStatus: "ACTIVE",
		},
		{
			name:               "identity changed while pending",
			getID:              "load-balancer-1",
			getProjectID:       "project-a",
			getTags:            []string{"owned-by=someone-else"},
			provisioningStatus: "PENDING_UPDATE",
		},
		{
			name:               "response ID changed",
			getID:              "load-balancer-2",
			getProjectID:       "project-a",
			getTags:            managedTags,
			provisioningStatus: "ACTIVE",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutations := 0
			handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet {
					mutations++
					http.Error(w, "mutation is not allowed", http.StatusMethodNotAllowed)
					return
				}
				switch request.URL.Path {
				case "/lbaas/loadbalancers":
					safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{map[string]any{
						"id": "load-balancer-1", "project_id": "project-a",
						"vip_port_id": "vip-port", "tags": managedTags,
					}}})
				case "/lbaas/loadbalancers/load-balancer-1":
					safetyWriteJSON(t, w, map[string]any{"loadbalancer": map[string]any{
						"id": test.getID, "project_id": test.getProjectID,
						"vip_port_id": "vip-port", "tags": test.getTags,
						"provisioning_status": test.provisioningStatus,
					}})
				default:
					t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
					http.Error(w, "unexpected request", http.StatusNotFound)
				}
			})
			provider := NewProvider(ServiceClients{
				LoadBalancer: safetyServiceClient(handler),
				ProjectID:    "project-a",
			}, ProviderConfig{})

			_, err := provider.DeleteGateway(context.Background(), identity.Value())
			if !errors.Is(err, cloud.ErrOwnershipConflict) {
				t.Fatalf("DeleteGateway() error = %v, want ownership conflict", err)
			}
			if mutations != 0 {
				t.Fatalf("DeleteGateway() mutations = %d, want 0", mutations)
			}
		})
	}
}

func TestDeleteGatewayValidatesEverySubplanBeforeMutation(t *testing.T) {
	identity := safetyTestIdentity(t)
	loadBalancerTags := safetyGatewayTags(t, identity, roleLoadBalancer)
	requests := &gatewayTransitionRequests{}
	graphHandler := safetyGatewayGraphHandler(t, identity, "member", newSafetyRequestLog())
	loadBalancerHandler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers":
			safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{gatewayTransitionLoadBalancer(loadBalancerTags, "ACTIVE")}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers/load-balancer-1":
			safetyWriteJSON(t, w, map[string]any{"loadbalancer": gatewayTransitionLoadBalancer(loadBalancerTags, "ACTIVE")})
		default:
			graphHandler.ServeHTTP(w, request)
		}
	})
	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(loadBalancerHandler),
		ProjectID:    "project-a",
	}, ProviderConfig{})

	_, err := provider.DeleteGateway(context.Background(), identity.Value())
	if !errors.Is(err, cloud.ErrOwnershipConflict) {
		t.Fatalf("DeleteGateway() error = %v, want ownership conflict from the pool subtree", err)
	}
	if got := requests.mutationPaths(); len(got) != 0 {
		t.Fatalf("DeleteGateway() mutations before complete plan validation = %v, want none", got)
	}
}

func TestBuildGatewayDeletionPlanValidatesProjectsAndParents(t *testing.T) {
	identity := safetyTestIdentity(t)
	tests := []struct {
		name   string
		target string
		mutate func(map[string]any)
	}{
		{name: "listener project", target: "listener", mutate: setDeletionTestField("project_id", "project-b")},
		{name: "listener parent", target: "listener", mutate: setDeletionTestField("loadbalancers", []any{map[string]any{"id": "load-balancer-2"}})},
		{name: "policy project", target: "policy", mutate: setDeletionTestField("project_id", "project-b")},
		{name: "policy parent", target: "policy", mutate: setDeletionTestField("listener_id", "listener-2")},
		{name: "rule project", target: "rule", mutate: setDeletionTestField("project_id", "project-b")},
		{name: "pool project", target: "pool", mutate: setDeletionTestField("project_id", "project-b")},
		{name: "pool parent", target: "pool", mutate: setDeletionTestField("loadbalancers", []any{map[string]any{"id": "load-balancer-2"}})},
		{name: "member project", target: "member", mutate: setDeletionTestField("project_id", "project-b")},
		{name: "member parent", target: "member", mutate: setDeletionTestField("pool_id", "pool-2")},
		{name: "monitor project", target: "monitor", mutate: setDeletionTestField("project_id", "project-b")},
		{name: "monitor parent", target: "monitor", mutate: setDeletionTestField("pools", []any{map[string]any{"id": "pool-2"}})},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := NewProvider(ServiceClients{
				LoadBalancer: safetyServiceClient(deletionValidationHandler(t, identity, test.target, test.mutate)),
				ProjectID:    "project-a",
			}, ProviderConfig{})

			_, err := provider.buildGatewayDeletionPlan(context.Background(), identity, "load-balancer-1")
			if !errors.Is(err, cloud.ErrOwnershipConflict) {
				t.Fatalf("buildGatewayDeletionPlan() error = %v, want ownership conflict", err)
			}
		})
	}
}

func setDeletionTestField(key string, value any) func(map[string]any) {
	return func(item map[string]any) {
		item[key] = value
	}
}

func deletionValidationHandler(t *testing.T, identity Identity, target string, mutate func(map[string]any)) http.Handler {
	t.Helper()
	tags := map[string][]string{
		"listener": safetyGatewayTags(t, identity, roleListener),
		"policy":   safetyRouteTags(t, identity, rolePolicyExact),
		"rule":     safetyRouteTags(t, identity, roleRulePath),
		"pool":     safetyRouteTags(t, identity, rolePool),
		"member":   safetyRouteTags(t, identity, roleMember),
		"monitor":  safetyRouteTags(t, identity, roleMonitor),
	}

	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Errorf("unexpected mutation: %s %s", request.Method, request.URL.RequestURI())
			http.Error(w, "mutation is not allowed", http.StatusMethodNotAllowed)
			return
		}
		resource := ""
		collection := ""
		var item map[string]any
		switch request.URL.Path {
		case "/lbaas/listeners":
			resource, collection = "listener", "listeners"
			item = map[string]any{
				"id": "listener-1", "project_id": "project-a", "tags": tags["listener"],
				"loadbalancers": []any{map[string]any{"id": "load-balancer-1"}},
			}
		case "/lbaas/l7policies":
			resource, collection = "policy", "l7policies"
			item = map[string]any{
				"id": "policy-1", "project_id": "project-a", "listener_id": "listener-1",
				"position": 1, "tags": tags["policy"],
			}
		case "/lbaas/l7policies/policy-1/rules":
			resource, collection = "rule", "rules"
			item = map[string]any{"id": "rule-1", "project_id": "project-a", "tags": tags["rule"]}
		case "/lbaas/pools":
			resource, collection = "pool", "pools"
			item = map[string]any{
				"id": "pool-1", "project_id": "project-a", "tags": tags["pool"],
				"loadbalancers": []any{map[string]any{"id": "load-balancer-1"}},
			}
		case "/lbaas/pools/pool-1/members":
			resource, collection = "member", "members"
			item = map[string]any{
				"id": "member-1", "project_id": "project-a", "pool_id": "pool-1",
				"address": "10.0.0.10", "protocol_port": 30080, "tags": tags["member"],
			}
		case "/lbaas/healthmonitors":
			resource, collection = "monitor", "healthmonitors"
			item = map[string]any{
				"id": "monitor-1", "project_id": "project-a", "tags": tags["monitor"],
				"pools": []any{map[string]any{"id": "pool-1"}},
			}
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		if resource == target {
			mutate(item)
		}
		safetyWriteJSON(t, w, map[string]any{collection: []any{item}})
	})
}
