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
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

func TestEnsureGatewayRepairsDisabledLoadBalancerFirst(t *testing.T) {
	identity := safetyTestIdentity(t)
	tags := safetyGatewayTags(t, identity, roleLoadBalancer)
	listenerTags := safetyGatewayTags(t, identity, roleListener)
	requests := &gatewayTransitionRequests{}
	loadBalancerHandler := gatewayDriftHandler(t, requests, tags, listenerTags, false, []any{map[string]any{
		"id": "listener-1", "protocol": "HTTP", "protocol_port": 80,
		"admin_state_up": true, "tags": listenerTags,
	}})
	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(loadBalancerHandler),
		Network:      safetyServiceClient(emptyFloatingIPHandler(t, requests)),
		ProjectID:    "project-a",
	}, ProviderConfig{PollInterval: 7 * time.Second})

	result, err := provider.EnsureGateway(context.Background(), gatewayTransitionSpec(identity))
	if err != nil {
		t.Fatalf("EnsureGateway() error = %v", err)
	}
	if result.Outcome.State != cloud.OutcomeProgressing || result.Outcome.RequeueAfter != 7*time.Second {
		t.Fatalf("EnsureGateway() outcome = %#v, want Progressing after 7s", result.Outcome)
	}
	want := []string{"/lbaas/loadbalancers/load-balancer-1"}
	if got := requests.mutationPaths(); !equalGatewayTransitionPaths(got, want) {
		t.Fatalf("mutating requests = %v, want %v", got, want)
	}
}

func TestEnsureGatewayDoesNotEnableLoadBalancerWithInvalidListener(t *testing.T) {
	identity := safetyTestIdentity(t)
	loadBalancerTags := safetyGatewayTags(t, identity, roleLoadBalancer)
	listenerTags := safetyGatewayTags(t, identity, roleListener)
	requests := &gatewayTransitionRequests{}
	loadBalancerHandler := gatewayDriftHandler(t, requests, loadBalancerTags, listenerTags, false, []any{map[string]any{
		"id": "listener-1", "protocol": "TCP", "protocol_port": 80,
		"admin_state_up": true, "tags": listenerTags,
	}})
	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(loadBalancerHandler),
		ProjectID:    "project-a",
	}, ProviderConfig{})

	_, err := provider.EnsureGateway(context.Background(), gatewayTransitionSpec(identity))
	if !errors.Is(err, cloud.ErrOwnershipConflict) {
		t.Fatalf("EnsureGateway() error = %v, want ownership conflict", err)
	}
	if got := requests.mutationPaths(); len(got) != 0 {
		t.Fatalf("mutating requests with an invalid listener = %v, want none", got)
	}
}

func TestEnsureGatewayDoesNotEnableLoadBalancerWithForeignFloatingIP(t *testing.T) {
	identity := safetyTestIdentity(t)
	loadBalancerTags := safetyGatewayTags(t, identity, roleLoadBalancer)
	listenerTags := safetyGatewayTags(t, identity, roleListener)
	requests := &gatewayTransitionRequests{}
	loadBalancerHandler := gatewayDriftHandler(t, requests, loadBalancerTags, listenerTags, false, []any{map[string]any{
		"id": "listener-1", "protocol": "HTTP", "protocol_port": 80,
		"admin_state_up": true, "tags": listenerTags,
	}})
	networkHandler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		if request.Method == http.MethodGet && request.URL.Path == "/floatingips" {
			safetyWriteJSON(t, w, map[string]any{"floatingips": []any{map[string]any{
				"id": "foreign-floating-ip", "project_id": "project-a", "port_id": "vip-port",
				"description": "owned by another controller",
			}}})
			return
		}
		t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
		http.Error(w, "unexpected request", http.StatusNotFound)
	})
	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(loadBalancerHandler),
		Network:      safetyServiceClient(networkHandler),
		ProjectID:    "project-a",
	}, ProviderConfig{})

	_, err := provider.EnsureGateway(context.Background(), gatewayTransitionSpec(identity))
	if !errors.Is(err, cloud.ErrOwnershipConflict) {
		t.Fatalf("EnsureGateway() error = %v, want ownership conflict", err)
	}
	if got := requests.mutationPaths(); len(got) != 0 {
		t.Fatalf("mutating requests with a foreign Floating IP = %v, want none", got)
	}
}

func TestEnsureGatewayDoesNotRepairBeforeGraphValidation(t *testing.T) {
	identity := safetyTestIdentity(t)
	tags := safetyGatewayTags(t, identity, roleLoadBalancer)
	requests := &gatewayTransitionRequests{}
	disabled := gatewayTransitionLoadBalancer(tags, "ACTIVE")
	disabled["admin_state_up"] = false
	foreignGraph := safetyGatewayGraphHandler(t, identity, "listener", newSafetyRequestLog())
	loadBalancerHandler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers":
			safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{disabled}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers/load-balancer-1":
			safetyWriteJSON(t, w, map[string]any{"loadbalancer": disabled})
		default:
			foreignGraph.ServeHTTP(w, request)
		}
	})
	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(loadBalancerHandler),
		ProjectID:    "project-a",
	}, ProviderConfig{})

	_, err := provider.EnsureGateway(context.Background(), gatewayTransitionSpec(identity))
	if !errors.Is(err, cloud.ErrOwnershipConflict) {
		t.Fatalf("EnsureGateway() error = %v, want ownership conflict", err)
	}
	if got := requests.mutationPaths(); len(got) != 0 {
		t.Fatalf("mutating requests before complete graph validation = %v, want none", got)
	}
}

func TestEnsureGatewayDefersAdministrativeRepairToBoundRoute(t *testing.T) {
	tests := []struct {
		name              string
		loadBalancerAdmin bool
		listenerAdmin     bool
		wantMessage       string
	}{
		{
			name: "load balancer", loadBalancerAdmin: false, listenerAdmin: true,
			wantMessage: "Waiting for HTTPRoute validation before enabling the Octavia load balancer",
		},
		{
			name: "listener", loadBalancerAdmin: true, listenerAdmin: false,
			wantMessage: "Waiting for HTTPRoute validation before enabling the Octavia listener",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			identity := safetyTestIdentity(t)
			gatewayValue := identity.Value()
			gatewayValue.RouteNamespace = ""
			gatewayValue.RouteName = ""
			gatewayValue.RouteUID = ""
			gatewayIdentity, err := NewIdentity(gatewayValue)
			if err != nil {
				t.Fatalf("NewIdentity() error = %v", err)
			}
			loadBalancerTags := safetyGatewayTags(t, identity, roleLoadBalancer)
			listenerTags := safetyGatewayTags(t, identity, roleListener)
			poolTags := safetyRouteTags(t, identity, rolePool)
			requests := &gatewayTransitionRequests{}
			baseHandler := gatewayDriftHandler(t, requests, loadBalancerTags, listenerTags, test.loadBalancerAdmin, []any{map[string]any{
				"id": "listener-1", "protocol": "HTTP", "protocol_port": 80,
				"admin_state_up": test.listenerAdmin, "tags": listenerTags,
			}})
			handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				switch {
				case request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools":
					requests.record(request)
					safetyWriteJSON(t, w, map[string]any{"pools": []any{map[string]any{
						"id": "pool-1", "tags": poolTags,
					}}})
				case request.Method == http.MethodGet && request.URL.Path == "/lbaas/healthmonitors":
					requests.record(request)
					safetyWriteJSON(t, w, map[string]any{"healthmonitors": []any{}})
				case request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools/pool-1/members":
					requests.record(request)
					safetyWriteJSON(t, w, map[string]any{"members": []any{}})
				default:
					baseHandler.ServeHTTP(w, request)
				}
			})
			provider := NewProvider(ServiceClients{
				LoadBalancer: safetyServiceClient(handler),
				Network:      safetyServiceClient(emptyFloatingIPHandler(t, requests)),
				ProjectID:    "project-a",
			}, ProviderConfig{PollInterval: 7 * time.Second})

			result, err := provider.EnsureGateway(context.Background(), gatewayTransitionSpec(gatewayIdentity))
			if err != nil {
				t.Fatalf("EnsureGateway() error = %v", err)
			}
			if result.Outcome.State != cloud.OutcomeProgressing || result.Outcome.Message != test.wantMessage {
				t.Fatalf("EnsureGateway() outcome = %#v, want %q", result.Outcome, test.wantMessage)
			}
			if got := requests.mutationPaths(); len(got) != 0 {
				t.Fatalf("mutating requests before exact HTTPRoute validation = %v, want none", got)
			}
		})
	}
}

func TestGetGatewayWaitsForAdministrativelyDisabledResources(t *testing.T) {
	tests := []struct {
		name              string
		loadBalancerAdmin bool
		listenerAdmin     bool
		wantMessage       string
	}{
		{name: "load balancer", loadBalancerAdmin: false, listenerAdmin: true, wantMessage: "Octavia load balancer is administratively disabled"},
		{name: "listener", loadBalancerAdmin: true, listenerAdmin: false, wantMessage: "Octavia listener is administratively disabled"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			identity := safetyTestIdentity(t)
			loadBalancerTags := safetyGatewayTags(t, identity, roleLoadBalancer)
			listenerTags := safetyGatewayTags(t, identity, roleListener)
			requests := &gatewayTransitionRequests{}
			handler := gatewayDriftHandler(t, requests, loadBalancerTags, listenerTags, test.loadBalancerAdmin, []any{map[string]any{
				"id": "listener-1", "protocol": "HTTP", "protocol_port": 80,
				"admin_state_up": test.listenerAdmin, "tags": listenerTags,
			}})
			provider := NewProvider(ServiceClients{
				LoadBalancer: safetyServiceClient(handler),
				ProjectID:    "project-a",
			}, ProviderConfig{PollInterval: 7 * time.Second})

			result, found, err := provider.GetGateway(context.Background(), identity.Value())
			if err != nil {
				t.Fatalf("GetGateway() error = %v", err)
			}
			if !found || result.Outcome.State != cloud.OutcomeProgressing ||
				result.Outcome.RequeueAfter != 7*time.Second || result.Outcome.Message != test.wantMessage {
				t.Fatalf("GetGateway() = %#v, %t, want Progressing after 7s", result, found)
			}
			if got := requests.mutationPaths(); len(got) != 0 {
				t.Fatalf("GetGateway() mutating requests = %v, want none", got)
			}
		})
	}
}

func TestEnsureGatewayRepairsDisabledListenerFirst(t *testing.T) {
	identity := safetyTestIdentity(t)
	loadBalancerTags := safetyGatewayTags(t, identity, roleLoadBalancer)
	listenerTags := safetyGatewayTags(t, identity, roleListener)
	requests := &gatewayTransitionRequests{}
	loadBalancerHandler := gatewayDriftHandler(t, requests, loadBalancerTags, listenerTags, true, []any{map[string]any{
		"id": "listener-1", "protocol": "HTTP", "protocol_port": 80,
		"admin_state_up": false, "tags": listenerTags,
	}})
	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(loadBalancerHandler),
		Network:      safetyServiceClient(emptyFloatingIPHandler(t, requests)),
		ProjectID:    "project-a",
	}, ProviderConfig{PollInterval: 7 * time.Second})

	result, err := provider.EnsureGateway(context.Background(), gatewayTransitionSpec(identity))
	if err != nil {
		t.Fatalf("EnsureGateway() error = %v", err)
	}
	if result.Outcome.State != cloud.OutcomeProgressing || result.Outcome.RequeueAfter != 7*time.Second {
		t.Fatalf("EnsureGateway() outcome = %#v, want Progressing after 7s", result.Outcome)
	}
	want := []string{"/lbaas/listeners/listener-1"}
	if got := requests.mutationPaths(); !equalGatewayTransitionPaths(got, want) {
		t.Fatalf("mutating requests = %v, want %v", got, want)
	}
}

func TestEnsureGatewayRecreatesDeletedListener(t *testing.T) {
	identity := safetyTestIdentity(t)
	loadBalancerTags := safetyGatewayTags(t, identity, roleLoadBalancer)
	listenerTags := safetyGatewayTags(t, identity, roleListener)
	requests := &gatewayTransitionRequests{}
	loadBalancerHandler := gatewayDriftHandler(t, requests, loadBalancerTags, listenerTags, true, nil)
	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(loadBalancerHandler),
		Network:      safetyServiceClient(emptyFloatingIPHandler(t, requests)),
		ProjectID:    "project-a",
	}, ProviderConfig{PollInterval: 7 * time.Second})

	result, err := provider.EnsureGateway(context.Background(), gatewayTransitionSpec(identity))
	if err != nil {
		t.Fatalf("EnsureGateway() error = %v", err)
	}
	if result.Outcome.State != cloud.OutcomeProgressing || result.Outcome.RequeueAfter != 7*time.Second {
		t.Fatalf("EnsureGateway() outcome = %#v, want Progressing after 7s", result.Outcome)
	}
	want := []string{"/lbaas/listeners"}
	if got := requests.mutationPaths(); !equalGatewayTransitionPaths(got, want) {
		t.Fatalf("mutating requests = %v, want %v", got, want)
	}
}

func TestEnsureGatewayCreatesOneResourcePerTransition(t *testing.T) {
	identity := safetyTestIdentity(t)
	requests := &gatewayTransitionRequests{}
	loadBalancerHandler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers":
			safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{}})
		case request.Method == http.MethodPost && request.URL.Path == "/lbaas/loadbalancers":
			w.WriteHeader(http.StatusCreated)
			safetyWriteJSON(t, w, map[string]any{"loadbalancer": map[string]any{
				"id": "load-balancer-1", "project_id": "project-a",
			}})
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})
	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(loadBalancerHandler),
		ProjectID:    "project-a",
	}, ProviderConfig{OperationTimeout: time.Hour, PollInterval: 7 * time.Second})

	result, err := provider.EnsureGateway(context.Background(), gatewayTransitionSpec(identity))
	if err != nil {
		t.Fatalf("EnsureGateway() error = %v", err)
	}
	if result.Outcome.State != cloud.OutcomeProgressing {
		t.Fatalf("EnsureGateway() outcome = %#v, want Progressing", result.Outcome)
	}
	if result.Outcome.RequeueAfter != 7*time.Second {
		t.Fatalf("EnsureGateway() RequeueAfter = %s, want 7s", result.Outcome.RequeueAfter)
	}
	if got := requests.mutationPaths(); len(got) != 1 || got[0] != "/lbaas/loadbalancers" {
		t.Fatalf("mutating requests = %v, want one load balancer create", got)
	}
}

func TestEnsureGatewayPendingReturnsWithoutMutation(t *testing.T) {
	identity := safetyTestIdentity(t)
	tags := safetyGatewayTags(t, identity, roleLoadBalancer)
	requests := &gatewayTransitionRequests{}
	loadBalancerHandler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		switch request.URL.Path {
		case "/lbaas/loadbalancers":
			safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{gatewayTransitionLoadBalancer(tags, "PENDING_UPDATE")}})
		case "/lbaas/loadbalancers/load-balancer-1":
			safetyWriteJSON(t, w, map[string]any{"loadbalancer": gatewayTransitionLoadBalancer(tags, "PENDING_UPDATE")})
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})
	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(loadBalancerHandler),
		ProjectID:    "project-a",
	}, ProviderConfig{OperationTimeout: time.Hour, PollInterval: time.Hour})
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	result, err := provider.EnsureGateway(ctx, gatewayTransitionSpec(identity))
	if err != nil {
		t.Fatalf("EnsureGateway() error = %v", err)
	}
	if result.Outcome.State != cloud.OutcomeProgressing {
		t.Fatalf("EnsureGateway() outcome = %#v, want Progressing", result.Outcome)
	}
	if got := requests.mutationPaths(); len(got) != 0 {
		t.Fatalf("mutating requests while load balancer is pending = %v, want none", got)
	}
	if got := requests.count(http.MethodGet, "/lbaas/loadbalancers/load-balancer-1"); got != 1 {
		t.Fatalf("load balancer observations = %d, want 1", got)
	}
}

func TestEnsureGatewayErrorStatusReturnsTypedFailure(t *testing.T) {
	identity := safetyTestIdentity(t)
	tags := safetyGatewayTags(t, identity, roleLoadBalancer)
	requests := &gatewayTransitionRequests{}
	loadBalancerHandler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		switch request.URL.Path {
		case "/lbaas/loadbalancers":
			safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{gatewayTransitionLoadBalancer(tags, "ERROR")}})
		case "/lbaas/loadbalancers/load-balancer-1":
			safetyWriteJSON(t, w, map[string]any{"loadbalancer": gatewayTransitionLoadBalancer(tags, "ERROR")})
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})
	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(loadBalancerHandler),
		ProjectID:    "project-a",
	}, ProviderConfig{})

	_, err := provider.EnsureGateway(context.Background(), gatewayTransitionSpec(identity))
	if !errors.Is(err, cloud.ErrResourceFailure) {
		t.Fatalf("EnsureGateway() error = %v, want resource failure", err)
	}
	category, ok := cloud.ErrorCategoryOf(err)
	if !ok || category != cloud.ErrorCategoryResourceFailure {
		t.Fatalf("EnsureGateway() error category = %q, %t, want %q", category, ok, cloud.ErrorCategoryResourceFailure)
	}
	if got := requests.mutationPaths(); len(got) != 0 {
		t.Fatalf("mutating requests after ERROR observation = %v, want none", got)
	}
}

func TestDeleteGatewayPendingReturnsWithoutMutation(t *testing.T) {
	identity := safetyTestIdentity(t)
	tags := safetyGatewayTags(t, identity, roleLoadBalancer)
	requests := &gatewayTransitionRequests{}
	loadBalancerHandler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		switch request.URL.Path {
		case "/lbaas/loadbalancers":
			safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{gatewayTransitionLoadBalancer(tags, "PENDING_DELETE")}})
		case "/lbaas/loadbalancers/load-balancer-1":
			safetyWriteJSON(t, w, map[string]any{"loadbalancer": gatewayTransitionLoadBalancer(tags, "PENDING_DELETE")})
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})
	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(loadBalancerHandler),
		ProjectID:    "project-a",
	}, ProviderConfig{OperationTimeout: time.Hour, PollInterval: 11 * time.Second})

	outcome, err := provider.DeleteGateway(context.Background(), identity.Value())
	if err != nil {
		t.Fatalf("DeleteGateway() error = %v", err)
	}
	if outcome.State != cloud.OutcomeProgressing || outcome.RequeueAfter != 11*time.Second {
		t.Fatalf("DeleteGateway() outcome = %#v, want Progressing after 11s", outcome)
	}
	if got := requests.mutationPaths(); len(got) != 0 {
		t.Fatalf("mutating requests while load balancer is pending = %v, want none", got)
	}
}

func TestDeleteGatewayExecutesOnlyFirstValidatedStep(t *testing.T) {
	identity := safetyTestIdentity(t)
	loadBalancerTags := safetyGatewayTags(t, identity, roleLoadBalancer)
	requests := &gatewayTransitionRequests{}
	graphHandler := safetyGatewayGraphHandler(t, identity, "", newSafetyRequestLog())
	loadBalancerHandler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers":
			safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{gatewayTransitionLoadBalancer(loadBalancerTags, "ACTIVE")}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers/load-balancer-1":
			safetyWriteJSON(t, w, map[string]any{"loadbalancer": gatewayTransitionLoadBalancer(loadBalancerTags, "ACTIVE")})
		case request.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			graphHandler.ServeHTTP(w, request)
		}
	})
	networkHandler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		if request.Method == http.MethodGet && request.URL.Path == "/floatingips" {
			safetyWriteJSON(t, w, map[string]any{"floatingips": []any{}})
			return
		}
		t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
		http.Error(w, "unexpected request", http.StatusNotFound)
	})
	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(loadBalancerHandler),
		Network:      safetyServiceClient(networkHandler),
		ProjectID:    "project-a",
	}, ProviderConfig{PollInterval: 5 * time.Second})

	outcome, err := provider.DeleteGateway(context.Background(), identity.Value())
	if err != nil {
		t.Fatalf("DeleteGateway() error = %v", err)
	}
	if outcome.State != cloud.OutcomeProgressing {
		t.Fatalf("DeleteGateway() outcome = %#v, want Progressing", outcome)
	}
	want := []string{"/lbaas/l7policies/policy-1/rules/rule-1"}
	if got := requests.mutationPaths(); !equalGatewayTransitionPaths(got, want) {
		t.Fatalf("mutating requests = %v, want %v", got, want)
	}
	if got := requests.count(http.MethodGet, "/lbaas/loadbalancers/load-balancer-1"); got != 1 {
		t.Fatalf("load balancer observations = %d, want 1", got)
	}
}

func gatewayTransitionSpec(identity Identity) cloud.GatewaySpec {
	return cloud.GatewaySpec{
		Identity:     identity.Value(),
		Provider:     "amphora",
		VIPSubnetID:  "vip-subnet",
		ListenerName: "http",
		ListenerPort: 80,
	}
}

func gatewayTransitionLoadBalancer(tags []string, provisioningStatus string) map[string]any {
	return map[string]any{
		"id":                  "load-balancer-1",
		"project_id":          "project-a",
		"provider":            "amphora",
		"vip_subnet_id":       "vip-subnet",
		"vip_port_id":         "vip-port",
		"vip_address":         "192.0.2.10",
		"provisioning_status": provisioningStatus,
		"admin_state_up":      true,
		"tags":                tags,
	}
}

func gatewayDriftHandler(
	t *testing.T,
	requests *gatewayTransitionRequests,
	loadBalancerTags []string,
	wantListenerTags []string,
	loadBalancerAdminState bool,
	listenerItems []any,
) http.Handler {
	t.Helper()
	if listenerItems == nil {
		listenerItems = []any{}
	}
	loadBalancer := gatewayTransitionLoadBalancer(loadBalancerTags, "ACTIVE")
	loadBalancer["admin_state_up"] = loadBalancerAdminState
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers":
			safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{loadBalancer}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers/load-balancer-1":
			safetyWriteJSON(t, w, map[string]any{"loadbalancer": loadBalancer})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/listeners":
			safetyWriteJSON(t, w, map[string]any{"listeners": listenerItems})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/l7policies":
			safetyWriteJSON(t, w, map[string]any{"l7policies": []any{}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools":
			safetyWriteJSON(t, w, map[string]any{"pools": []any{}})
		case request.Method == http.MethodPut && request.URL.Path == "/lbaas/loadbalancers/load-balancer-1":
			var body struct {
				LoadBalancer struct {
					AdminStateUp *bool `json:"admin_state_up"`
				} `json:"loadbalancer"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("decode load balancer update: %v", err)
			}
			if body.LoadBalancer.AdminStateUp == nil || !*body.LoadBalancer.AdminStateUp {
				t.Fatalf("load balancer update = %#v, want admin_state_up true", body.LoadBalancer)
			}
			safetyWriteJSON(t, w, map[string]any{"loadbalancer": map[string]any{"id": "load-balancer-1", "admin_state_up": true}})
		case request.Method == http.MethodPut && request.URL.Path == "/lbaas/listeners/listener-1":
			var body struct {
				Listener struct {
					AdminStateUp *bool `json:"admin_state_up"`
				} `json:"listener"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("decode listener update: %v", err)
			}
			if body.Listener.AdminStateUp == nil || !*body.Listener.AdminStateUp {
				t.Fatalf("listener update = %#v, want admin_state_up true", body.Listener)
			}
			safetyWriteJSON(t, w, map[string]any{"listener": map[string]any{"id": "listener-1", "admin_state_up": true}})
		case request.Method == http.MethodPost && request.URL.Path == "/lbaas/listeners":
			var body struct {
				Listener struct {
					LoadBalancerID string   `json:"loadbalancer_id"`
					Protocol       string   `json:"protocol"`
					ProtocolPort   int      `json:"protocol_port"`
					AdminStateUp   *bool    `json:"admin_state_up"`
					Tags           []string `json:"tags"`
				} `json:"listener"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("decode listener create: %v", err)
			}
			if body.Listener.LoadBalancerID != "load-balancer-1" || body.Listener.Protocol != "HTTP" ||
				body.Listener.ProtocolPort != 80 || body.Listener.AdminStateUp == nil ||
				!*body.Listener.AdminStateUp || !slices.Equal(body.Listener.Tags, wantListenerTags) {
				t.Fatalf("listener create = %#v, want enabled HTTP listener on port 80", body.Listener)
			}
			w.WriteHeader(http.StatusCreated)
			safetyWriteJSON(t, w, map[string]any{"listener": map[string]any{"id": "listener-1"}})
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})
}

func emptyFloatingIPHandler(t *testing.T, requests *gatewayTransitionRequests) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		if request.Method == http.MethodGet && request.URL.Path == "/floatingips" {
			safetyWriteJSON(t, w, map[string]any{"floatingips": []any{}})
			return
		}
		t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
		http.Error(w, "unexpected request", http.StatusNotFound)
	})
}

type gatewayTransitionRequest struct {
	method string
	path   string
}

type gatewayTransitionRequests struct {
	mutex    sync.Mutex
	requests []gatewayTransitionRequest
}

func (r *gatewayTransitionRequests) record(request *http.Request) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.requests = append(r.requests, gatewayTransitionRequest{method: request.Method, path: request.URL.Path})
}

func (r *gatewayTransitionRequests) count(method, path string) int {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	count := 0
	for _, request := range r.requests {
		if request.method == method && request.path == path {
			count++
		}
	}
	return count
}

func (r *gatewayTransitionRequests) mutationPaths() []string {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	var paths []string
	for _, request := range r.requests {
		if request.method != http.MethodGet {
			paths = append(paths, request.path)
		}
	}
	return paths
}

func equalGatewayTransitionPaths(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range want {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
