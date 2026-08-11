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

package openstack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

func TestEnsureRoutePendingReturnsWithoutMutation(t *testing.T) {
	identity := safetyTestIdentity(t)
	loadBalancerTags := safetyGatewayTags(t, identity, roleLoadBalancer)
	requests := &gatewayTransitionRequests{}
	loadBalancerHandler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		switch request.URL.Path {
		case "/lbaas/loadbalancers":
			safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{
				gatewayTransitionLoadBalancer(loadBalancerTags, "PENDING_UPDATE"),
			}})
		case "/lbaas/loadbalancers/load-balancer-1":
			safetyWriteJSON(t, w, map[string]any{"loadbalancer": gatewayTransitionLoadBalancer(loadBalancerTags, "PENDING_UPDATE")})
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})
	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(loadBalancerHandler),
		ProjectID:    "project-a",
	}, ProviderConfig{OperationTimeout: time.Hour, PollInterval: 9 * time.Second})

	result, err := provider.EnsureRoute(context.Background(), routeTransitionSpec(identity))
	if err != nil {
		t.Fatalf("EnsureRoute() error = %v", err)
	}
	if result.Outcome.State != cloud.OutcomeProgressing || result.Outcome.RequeueAfter != 9*time.Second {
		t.Fatalf("EnsureRoute() outcome = %#v, want Progressing after 9s", result.Outcome)
	}
	if got := requests.mutationPaths(); len(got) != 0 {
		t.Fatalf("mutating requests while load balancer is pending = %v, want none", got)
	}
	if got := requests.count(http.MethodGet, "/lbaas/loadbalancers/load-balancer-1"); got != 1 {
		t.Fatalf("load balancer observations = %d, want 1", got)
	}
}

func TestEnsureRouteCreatesOnlyPoolInFirstTransition(t *testing.T) {
	identity := safetyTestIdentity(t)
	loadBalancerTags := safetyGatewayTags(t, identity, roleLoadBalancer)
	listenerTags := safetyGatewayTags(t, identity, roleListener)
	requests := &gatewayTransitionRequests{}
	loadBalancerHandler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers":
			safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{
				gatewayTransitionLoadBalancer(loadBalancerTags, "ACTIVE"),
			}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers/load-balancer-1":
			status := "ACTIVE"
			if len(requests.mutationPaths()) != 0 {
				status = "PENDING_CREATE"
			}
			safetyWriteJSON(t, w, map[string]any{"loadbalancer": gatewayTransitionLoadBalancer(loadBalancerTags, status)})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/listeners":
			safetyWriteJSON(t, w, map[string]any{"listeners": []any{map[string]any{
				"id": "listener-1", "protocol": "HTTP", "tags": listenerTags,
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/l7policies":
			safetyWriteJSON(t, w, map[string]any{"l7policies": []any{}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools":
			safetyWriteJSON(t, w, map[string]any{"pools": []any{}})
		case request.Method == http.MethodPost && request.URL.Path == "/lbaas/pools":
			w.WriteHeader(http.StatusCreated)
			safetyWriteJSON(t, w, map[string]any{"pool": map[string]any{"id": "pool-1"}})
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})
	networkHandler := routeTransitionEmptyFloatingIPHandler(t, requests)
	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(loadBalancerHandler),
		Network:      safetyServiceClient(networkHandler),
		ProjectID:    "project-a",
	}, ProviderConfig{OperationTimeout: time.Hour, PollInterval: time.Hour})
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	result, err := provider.EnsureRoute(ctx, routeTransitionSpec(identity))
	if err != nil {
		t.Fatalf("EnsureRoute() error = %v", err)
	}
	if result.Outcome.State != cloud.OutcomeProgressing {
		t.Fatalf("EnsureRoute() outcome = %#v, want Progressing", result.Outcome)
	}
	want := []string{"/lbaas/pools"}
	if got := requests.mutationPaths(); !equalGatewayTransitionPaths(got, want) {
		t.Fatalf("mutating requests = %v, want %v", got, want)
	}
}

func TestEnsureRouteChoosesStaleMemberDeterministically(t *testing.T) {
	identity := safetyTestIdentity(t)
	loadBalancerTags := safetyGatewayTags(t, identity, roleLoadBalancer)
	listenerTags := safetyGatewayTags(t, identity, roleListener)
	poolTags := safetyRouteTags(t, identity, rolePool)
	memberTags := safetyRouteTags(t, identity, roleMember)
	requests := &gatewayTransitionRequests{}
	loadBalancerHandler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers":
			safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{
				gatewayTransitionLoadBalancer(loadBalancerTags, "ACTIVE"),
			}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers/load-balancer-1":
			status := "ACTIVE"
			if len(requests.mutationPaths()) != 0 {
				status = "PENDING_UPDATE"
			}
			safetyWriteJSON(t, w, map[string]any{"loadbalancer": gatewayTransitionLoadBalancer(loadBalancerTags, status)})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/listeners":
			safetyWriteJSON(t, w, map[string]any{"listeners": []any{map[string]any{
				"id": "listener-1", "protocol": "HTTP", "tags": listenerTags,
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/l7policies":
			safetyWriteJSON(t, w, map[string]any{"l7policies": []any{}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools":
			safetyWriteJSON(t, w, map[string]any{"pools": []any{map[string]any{
				"id": "pool-1", "protocol": "HTTP", "lb_algorithm": "ROUND_ROBIN", "admin_state_up": true, "tags": poolTags,
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools/pool-1/members":
			safetyWriteJSON(t, w, map[string]any{"members": []any{
				map[string]any{"id": "member-z", "address": "10.0.0.20", "protocol_port": 30080, "subnet_id": "member-subnet", "tags": memberTags},
				map[string]any{"id": "member-a", "address": "10.0.0.10", "protocol_port": 30080, "subnet_id": "member-subnet", "tags": memberTags},
			}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/healthmonitors":
			safetyWriteJSON(t, w, map[string]any{"healthmonitors": []any{}})
		case request.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})
	networkHandler := routeTransitionEmptyFloatingIPHandler(t, requests)
	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(loadBalancerHandler),
		Network:      safetyServiceClient(networkHandler),
		ProjectID:    "project-a",
	}, ProviderConfig{OperationTimeout: time.Hour, PollInterval: time.Hour})
	spec := routeTransitionSpec(identity)
	spec.Members = []cloud.Member{{Address: "10.0.0.30", Port: 30080}}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	result, err := provider.EnsureRoute(ctx, spec)
	if err != nil {
		t.Fatalf("EnsureRoute() error = %v", err)
	}
	if result.Outcome.State != cloud.OutcomeProgressing {
		t.Fatalf("EnsureRoute() outcome = %#v, want Progressing", result.Outcome)
	}
	want := []string{"/lbaas/pools/pool-1/members/member-a"}
	if got := requests.mutationPaths(); !equalGatewayTransitionPaths(got, want) {
		t.Fatalf("mutating requests = %v, want %v", got, want)
	}
}

func TestEnsureRouteRepairsDisabledMemberBeforeReportingReady(t *testing.T) {
	identity := safetyTestIdentity(t)
	loadBalancerTags := safetyGatewayTags(t, identity, roleLoadBalancer)
	listenerTags := safetyGatewayTags(t, identity, roleListener)
	poolTags := safetyRouteTags(t, identity, rolePool)
	memberTags := safetyRouteTags(t, identity, roleMember)
	monitorTags := safetyRouteTags(t, identity, roleMonitor)
	policyTags := safetyRouteTags(t, identity, rolePolicyExact)
	ruleTags := safetyRouteTags(t, identity, roleRulePath)
	requests := &gatewayTransitionRequests{}
	memberEnabled := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers":
			safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{
				gatewayTransitionLoadBalancer(loadBalancerTags, "ACTIVE"),
			}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers/load-balancer-1":
			safetyWriteJSON(t, w, map[string]any{"loadbalancer": gatewayTransitionLoadBalancer(loadBalancerTags, "ACTIVE")})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/listeners":
			safetyWriteJSON(t, w, map[string]any{"listeners": []any{map[string]any{
				"id": "listener-1", "protocol": "HTTP", "tags": listenerTags,
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools":
			safetyWriteJSON(t, w, map[string]any{"pools": []any{map[string]any{
				"id": "pool-1", "protocol": "HTTP", "lb_algorithm": "ROUND_ROBIN",
				"admin_state_up": true, "tags": poolTags,
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools/pool-1/members":
			safetyWriteJSON(t, w, map[string]any{"members": []any{map[string]any{
				"id": "member-1", "address": "10.0.0.10", "protocol_port": 30080,
				"subnet_id": "member-subnet", "admin_state_up": memberEnabled, "tags": memberTags,
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/healthmonitors":
			safetyWriteJSON(t, w, map[string]any{"healthmonitors": []any{map[string]any{
				"id": "monitor-1", "type": "HTTP", "delay": 10, "timeout": 5,
				"max_retries": 3, "max_retries_down": 3, "url_path": "/healthz",
				"http_method": "GET", "expected_codes": "200-399", "admin_state_up": true,
				"tags": monitorTags,
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/l7policies":
			safetyWriteJSON(t, w, map[string]any{"l7policies": []any{map[string]any{
				"id": "policy-1", "listener_id": "listener-1", "action": "REDIRECT_TO_POOL",
				"redirect_pool_id": "pool-1", "position": 1, "admin_state_up": true,
				"tags": policyTags,
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/l7policies/policy-1/rules":
			safetyWriteJSON(t, w, map[string]any{"rules": []any{map[string]any{
				"id": "rule-path", "type": "PATH", "compare_type": "EQUAL_TO",
				"value": "/api", "admin_state_up": true, "tags": ruleTags,
			}}})
		case request.Method == http.MethodPut && request.URL.Path == "/lbaas/pools/pool-1/members/member-1":
			var body struct {
				Member struct {
					AdminStateUp *bool `json:"admin_state_up"`
				} `json:"member"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode member update: %v", err)
			}
			if body.Member.AdminStateUp == nil || !*body.Member.AdminStateUp {
				t.Errorf("member update body = %#v, want admin_state_up true", body)
			}
			memberEnabled = true
			safetyWriteJSON(t, w, map[string]any{"member": map[string]any{"id": "member-1"}})
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})
	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(handler),
		ProjectID:    "project-a",
	}, ProviderConfig{PollInterval: time.Second})
	spec := routeTransitionSpec(identity)
	spec.Hostname = ""

	first, err := provider.EnsureRoute(context.Background(), spec)
	if err != nil {
		t.Fatalf("first EnsureRoute() error = %v", err)
	}
	if first.Outcome.State != cloud.OutcomeProgressing {
		t.Fatalf("first EnsureRoute() outcome = %#v, want Progressing", first.Outcome)
	}
	second, err := provider.EnsureRoute(context.Background(), spec)
	if err != nil {
		t.Fatalf("second EnsureRoute() error = %v", err)
	}
	if second.Outcome.State != cloud.OutcomeReady {
		t.Fatalf("second EnsureRoute() outcome = %#v, want Ready", second.Outcome)
	}
	want := []string{"/lbaas/pools/pool-1/members/member-1"}
	if got := requests.mutationPaths(); !equalGatewayTransitionPaths(got, want) {
		t.Fatalf("mutating requests = %v, want %v", got, want)
	}
}

func TestEnsureRouteOperationTimeoutStopsObservation(t *testing.T) {
	identity := safetyTestIdentity(t)
	requests := &gatewayTransitionRequests{}
	loadBalancerClient := safetyServiceClient(http.NotFoundHandler())
	loadBalancerClient.HTTPClient.Transport = safetyRoundTripper(func(request *http.Request) (*http.Response, error) {
		requests.record(request)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	provider := NewProvider(ServiceClients{
		LoadBalancer: loadBalancerClient,
		ProjectID:    "project-a",
	}, ProviderConfig{OperationTimeout: 20 * time.Millisecond})

	_, err := provider.EnsureRoute(context.Background(), routeTransitionSpec(identity))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("EnsureRoute() error = %v, want context deadline exceeded", err)
	}
	if !errors.Is(err, cloud.ErrTimeout) {
		t.Fatalf("EnsureRoute() error = %v, want timeout category", err)
	}
	if got := requests.mutationPaths(); len(got) != 0 {
		t.Fatalf("mutating requests after operation timeout = %v, want none", got)
	}
}

func TestEnsureRouteConvergesWithOneMutationPerCall(t *testing.T) {
	identity := safetyTestIdentity(t)
	loadBalancerTags := safetyGatewayTags(t, identity, roleLoadBalancer)
	listenerTags := safetyGatewayTags(t, identity, roleListener)
	poolTags := safetyRouteTags(t, identity, rolePool)
	memberTags := safetyRouteTags(t, identity, roleMember)
	monitorTags := safetyRouteTags(t, identity, roleMonitor)
	policyTags := safetyRouteTags(t, identity, rolePolicyExact)
	pathRuleTags := safetyRouteTags(t, identity, roleRulePath)
	hostRuleTags := safetyRouteTags(t, identity, roleRuleHost)
	requests := &gatewayTransitionRequests{}

	poolCreated := false
	memberCreated := false
	monitorCreated := false
	policyCreated := false
	pathRuleCreated := false
	hostRuleCreated := false
	policyEnabled := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers":
			safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{
				gatewayTransitionLoadBalancer(loadBalancerTags, "ACTIVE"),
			}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers/load-balancer-1":
			safetyWriteJSON(t, w, map[string]any{"loadbalancer": gatewayTransitionLoadBalancer(loadBalancerTags, "ACTIVE")})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/listeners":
			safetyWriteJSON(t, w, map[string]any{"listeners": []any{map[string]any{
				"id": "listener-1", "protocol": "HTTP", "tags": listenerTags,
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools":
			items := []any{}
			if poolCreated {
				items = append(items, map[string]any{
					"id": "pool-1", "protocol": "HTTP", "lb_algorithm": "ROUND_ROBIN",
					"admin_state_up": true, "tags": poolTags,
				})
			}
			safetyWriteJSON(t, w, map[string]any{"pools": items})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools/pool-1/members":
			items := []any{}
			if memberCreated {
				items = append(items, map[string]any{
					"id": "member-1", "address": "10.0.0.10", "protocol_port": 30080,
					"subnet_id": "member-subnet", "admin_state_up": true, "tags": memberTags,
				})
			}
			safetyWriteJSON(t, w, map[string]any{"members": items})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/healthmonitors":
			items := []any{}
			if monitorCreated {
				items = append(items, map[string]any{
					"id": "monitor-1", "type": "HTTP", "delay": 10, "timeout": 5,
					"max_retries": 3, "max_retries_down": 3, "url_path": "/healthz",
					"http_method": "GET", "expected_codes": "200-399", "admin_state_up": true,
					"tags": monitorTags,
				})
			}
			safetyWriteJSON(t, w, map[string]any{"healthmonitors": items})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/l7policies":
			items := []any{}
			if policyCreated {
				items = append(items, map[string]any{
					"id": "policy-1", "listener_id": "listener-1", "action": "REDIRECT_TO_POOL",
					"redirect_pool_id": "pool-1", "position": 1, "admin_state_up": policyEnabled,
					"tags": policyTags,
				})
			}
			safetyWriteJSON(t, w, map[string]any{"l7policies": items})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/l7policies/policy-1/rules":
			items := []any{}
			if pathRuleCreated {
				items = append(items, map[string]any{
					"id": "rule-path", "type": "PATH", "compare_type": "EQUAL_TO",
					"value": "/api", "admin_state_up": true, "tags": pathRuleTags,
				})
			}
			if hostRuleCreated {
				items = append(items, map[string]any{
					"id": "rule-host", "type": "HOST_NAME", "compare_type": "EQUAL_TO",
					"value": "api.example.com", "admin_state_up": true, "tags": hostRuleTags,
				})
			}
			safetyWriteJSON(t, w, map[string]any{"rules": items})
		case request.Method == http.MethodPost && request.URL.Path == "/lbaas/pools":
			poolCreated = true
			w.WriteHeader(http.StatusCreated)
			safetyWriteJSON(t, w, map[string]any{"pool": map[string]any{"id": "pool-1"}})
		case request.Method == http.MethodPost && request.URL.Path == "/lbaas/pools/pool-1/members":
			memberCreated = true
			w.WriteHeader(http.StatusCreated)
			safetyWriteJSON(t, w, map[string]any{"member": map[string]any{"id": "member-1"}})
		case request.Method == http.MethodPost && request.URL.Path == "/lbaas/healthmonitors":
			monitorCreated = true
			w.WriteHeader(http.StatusCreated)
			safetyWriteJSON(t, w, map[string]any{"healthmonitor": map[string]any{"id": "monitor-1"}})
		case request.Method == http.MethodPost && request.URL.Path == "/lbaas/l7policies":
			policyCreated = true
			w.WriteHeader(http.StatusCreated)
			safetyWriteJSON(t, w, map[string]any{"l7policy": map[string]any{"id": "policy-1"}})
		case request.Method == http.MethodPost && request.URL.Path == "/lbaas/l7policies/policy-1/rules":
			if pathRuleCreated {
				hostRuleCreated = true
			} else {
				pathRuleCreated = true
			}
			w.WriteHeader(http.StatusCreated)
			safetyWriteJSON(t, w, map[string]any{"rule": map[string]any{"id": "rule-created"}})
		case request.Method == http.MethodPut && request.URL.Path == "/lbaas/l7policies/policy-1":
			policyEnabled = true
			safetyWriteJSON(t, w, map[string]any{"l7policy": map[string]any{"id": "policy-1"}})
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})
	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(handler),
		ProjectID:    "project-a",
	}, ProviderConfig{PollInterval: time.Second})

	ready := false
	for attempt := 0; attempt < 10; attempt++ {
		before := len(requests.mutationPaths())
		result, err := provider.EnsureRoute(context.Background(), routeTransitionSpec(identity))
		if err != nil {
			t.Fatalf("EnsureRoute() attempt %d error = %v", attempt+1, err)
		}
		after := len(requests.mutationPaths())
		if after-before > 1 {
			t.Fatalf("EnsureRoute() attempt %d made %d mutations", attempt+1, after-before)
		}
		if result.Outcome.State == cloud.OutcomeReady {
			ready = true
			break
		}
	}
	if !ready {
		t.Fatal("EnsureRoute() did not converge")
	}
	want := []string{
		"/lbaas/pools",
		"/lbaas/pools/pool-1/members",
		"/lbaas/healthmonitors",
		"/lbaas/l7policies",
		"/lbaas/l7policies/policy-1/rules",
		"/lbaas/l7policies/policy-1/rules",
		"/lbaas/l7policies/policy-1",
	}
	if got := requests.mutationPaths(); !equalGatewayTransitionPaths(got, want) {
		t.Fatalf("route mutation order = %v, want %v", got, want)
	}
}

func TestEnsureRouteValidatesEveryDescendantBeforeMutation(t *testing.T) {
	identity := safetyTestIdentity(t)
	loadBalancerTags := safetyGatewayTags(t, identity, roleLoadBalancer)
	listenerTags := safetyGatewayTags(t, identity, roleListener)
	poolTags := safetyRouteTags(t, identity, rolePool)
	memberTags := safetyRouteTags(t, identity, roleMember)
	requests := &gatewayTransitionRequests{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers":
			safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{
				gatewayTransitionLoadBalancer(loadBalancerTags, "ACTIVE"),
			}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers/load-balancer-1":
			safetyWriteJSON(t, w, map[string]any{"loadbalancer": gatewayTransitionLoadBalancer(loadBalancerTags, "ACTIVE")})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/listeners":
			safetyWriteJSON(t, w, map[string]any{"listeners": []any{map[string]any{
				"id": "listener-1", "protocol": "HTTP", "tags": listenerTags,
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools":
			safetyWriteJSON(t, w, map[string]any{"pools": []any{map[string]any{
				"id": "pool-1", "protocol": "HTTP", "lb_algorithm": "ROUND_ROBIN",
				"admin_state_up": true, "tags": poolTags,
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools/pool-1/members":
			safetyWriteJSON(t, w, map[string]any{"members": []any{
				map[string]any{
					"id": "owned-member", "address": "10.0.0.10", "protocol_port": 30080,
					"subnet_id": "member-subnet", "tags": memberTags,
				},
				map[string]any{
					"id": "foreign-member", "address": "10.0.0.20", "protocol_port": 30080,
					"subnet_id": "member-subnet", "tags": []string{"owned-by=another-controller"},
				},
			}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/healthmonitors":
			safetyWriteJSON(t, w, map[string]any{"healthmonitors": []any{}})
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})
	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(handler),
		ProjectID:    "project-a",
	}, ProviderConfig{})
	spec := routeTransitionSpec(identity)
	spec.Members = []cloud.Member{{Address: "10.0.0.30", Port: 30080}}

	_, err := provider.EnsureRoute(context.Background(), spec)
	if !errors.Is(err, cloud.ErrOwnershipConflict) {
		t.Fatalf("EnsureRoute() error = %v, want ownership conflict", err)
	}
	if got := requests.mutationPaths(); len(got) != 0 {
		t.Fatalf("mutations before complete ownership validation = %v, want none", got)
	}
}

func TestEnsureRouteRediscoversPoolAfterLostCreateResponse(t *testing.T) {
	identity := safetyTestIdentity(t)
	loadBalancerTags := safetyGatewayTags(t, identity, roleLoadBalancer)
	listenerTags := safetyGatewayTags(t, identity, roleListener)
	poolTags := safetyRouteTags(t, identity, rolePool)
	requests := &gatewayTransitionRequests{}
	poolCreated := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers":
			safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{
				gatewayTransitionLoadBalancer(loadBalancerTags, "ACTIVE"),
			}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers/load-balancer-1":
			safetyWriteJSON(t, w, map[string]any{"loadbalancer": gatewayTransitionLoadBalancer(loadBalancerTags, "ACTIVE")})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/listeners":
			safetyWriteJSON(t, w, map[string]any{"listeners": []any{map[string]any{
				"id": "listener-1", "protocol": "HTTP", "tags": listenerTags,
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools":
			items := []any{}
			if poolCreated {
				items = append(items, map[string]any{
					"id": "pool-1", "protocol": "HTTP", "lb_algorithm": "ROUND_ROBIN",
					"admin_state_up": true, "tags": poolTags,
				})
			}
			safetyWriteJSON(t, w, map[string]any{"pools": items})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools/pool-1/members":
			safetyWriteJSON(t, w, map[string]any{"members": []any{}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/healthmonitors":
			safetyWriteJSON(t, w, map[string]any{"healthmonitors": []any{}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/l7policies":
			safetyWriteJSON(t, w, map[string]any{"l7policies": []any{}})
		case request.Method == http.MethodPost && request.URL.Path == "/lbaas/pools":
			poolCreated = true
			http.Error(w, "connection closed after create", http.StatusInternalServerError)
		case request.Method == http.MethodPost && request.URL.Path == "/lbaas/pools/pool-1/members":
			w.WriteHeader(http.StatusCreated)
			safetyWriteJSON(t, w, map[string]any{"member": map[string]any{"id": "member-1"}})
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})
	clients := ServiceClients{LoadBalancer: safetyServiceClient(handler), ProjectID: "project-a"}
	spec := routeTransitionSpec(identity)

	firstProvider := NewProvider(clients, ProviderConfig{})
	if _, err := firstProvider.EnsureRoute(context.Background(), spec); err == nil {
		t.Fatal("first EnsureRoute() error = nil, want lost create response")
	}
	secondProvider := NewProvider(clients, ProviderConfig{})
	result, err := secondProvider.EnsureRoute(context.Background(), spec)
	if err != nil {
		t.Fatalf("second EnsureRoute() error = %v", err)
	}
	if result.Outcome.State != cloud.OutcomeProgressing {
		t.Fatalf("second EnsureRoute() outcome = %#v, want Progressing", result.Outcome)
	}
	if got := requests.count(http.MethodPost, "/lbaas/pools"); got != 1 {
		t.Fatalf("pool create requests = %d, want 1 after provider restart", got)
	}
	want := []string{"/lbaas/pools", "/lbaas/pools/pool-1/members"}
	if got := requests.mutationPaths(); !equalGatewayTransitionPaths(got, want) {
		t.Fatalf("mutating requests = %v, want %v", got, want)
	}
}

func TestDeleteRoutePendingReturnsWithoutMutation(t *testing.T) {
	identity := safetyTestIdentity(t)
	loadBalancerTags := safetyGatewayTags(t, identity, roleLoadBalancer)
	requests := &gatewayTransitionRequests{}
	loadBalancerHandler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		switch request.URL.Path {
		case "/lbaas/loadbalancers":
			safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{
				gatewayTransitionLoadBalancer(loadBalancerTags, "PENDING_DELETE"),
			}})
		case "/lbaas/loadbalancers/load-balancer-1":
			safetyWriteJSON(t, w, map[string]any{"loadbalancer": gatewayTransitionLoadBalancer(loadBalancerTags, "PENDING_DELETE")})
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})
	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(loadBalancerHandler),
		ProjectID:    "project-a",
	}, ProviderConfig{OperationTimeout: time.Hour, PollInterval: 13 * time.Second})

	outcome, err := provider.DeleteRoute(context.Background(), identity.value)
	if err != nil {
		t.Fatalf("DeleteRoute() error = %v", err)
	}
	if outcome.State != cloud.OutcomeProgressing || outcome.RequeueAfter != 13*time.Second {
		t.Fatalf("DeleteRoute() outcome = %#v, want Progressing after 13s", outcome)
	}
	if got := requests.mutationPaths(); len(got) != 0 {
		t.Fatalf("mutating requests while load balancer is pending = %v, want none", got)
	}
}

func TestDeleteRouteConvergesInDeterministicOrder(t *testing.T) {
	identity := safetyTestIdentity(t)
	loadBalancerTags := safetyGatewayTags(t, identity, roleLoadBalancer)
	listenerTags := safetyGatewayTags(t, identity, roleListener)
	poolTags := safetyRouteTags(t, identity, rolePool)
	memberTags := safetyRouteTags(t, identity, roleMember)
	monitorTags := safetyRouteTags(t, identity, roleMonitor)
	policyTags := safetyRouteTags(t, identity, rolePolicyExact)
	pathRuleTags := safetyRouteTags(t, identity, roleRulePath)
	hostRuleTags := safetyRouteTags(t, identity, roleRuleHost)
	requests := &gatewayTransitionRequests{}
	poolExists := true
	memberExists := true
	monitorExists := true
	policyExists := true
	policyEnabled := true
	pathRuleExists := true
	hostRuleExists := true
	loadBalancerHandler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers":
			safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{
				gatewayTransitionLoadBalancer(loadBalancerTags, "ACTIVE"),
			}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers/load-balancer-1":
			safetyWriteJSON(t, w, map[string]any{"loadbalancer": gatewayTransitionLoadBalancer(loadBalancerTags, "ACTIVE")})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/listeners":
			safetyWriteJSON(t, w, map[string]any{"listeners": []any{map[string]any{
				"id": "listener-1", "protocol": "HTTP", "tags": listenerTags,
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/l7policies":
			items := []any{}
			if policyExists {
				items = append(items, map[string]any{
					"id": "policy-1", "position": 1, "admin_state_up": policyEnabled, "tags": policyTags,
				})
			}
			safetyWriteJSON(t, w, map[string]any{"l7policies": items})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/l7policies/policy-1/rules":
			items := []any{}
			if pathRuleExists {
				items = append(items, map[string]any{"id": "rule-z", "tags": pathRuleTags})
			}
			if hostRuleExists {
				items = append(items, map[string]any{"id": "rule-a", "tags": hostRuleTags})
			}
			safetyWriteJSON(t, w, map[string]any{"rules": items})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools":
			items := []any{}
			if poolExists {
				items = append(items, map[string]any{"id": "pool-1", "tags": poolTags})
			}
			safetyWriteJSON(t, w, map[string]any{"pools": items})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/healthmonitors":
			items := []any{}
			if monitorExists {
				items = append(items, map[string]any{"id": "monitor-1", "tags": monitorTags})
			}
			safetyWriteJSON(t, w, map[string]any{"healthmonitors": items})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools/pool-1/members":
			items := []any{}
			if memberExists {
				items = append(items, map[string]any{
					"id": "member-1", "address": "10.0.0.10", "protocol_port": 30080, "tags": memberTags,
				})
			}
			safetyWriteJSON(t, w, map[string]any{"members": items})
		case request.Method == http.MethodPut && request.URL.Path == "/lbaas/l7policies/policy-1":
			policyEnabled = false
			safetyWriteJSON(t, w, map[string]any{"l7policy": map[string]any{"id": "policy-1"}})
		case request.Method == http.MethodDelete && request.URL.Path == "/lbaas/l7policies/policy-1/rules/rule-a":
			hostRuleExists = false
			w.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodDelete && request.URL.Path == "/lbaas/l7policies/policy-1/rules/rule-z":
			pathRuleExists = false
			w.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodDelete && request.URL.Path == "/lbaas/l7policies/policy-1":
			policyExists = false
			w.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodDelete && request.URL.Path == "/lbaas/healthmonitors/monitor-1":
			monitorExists = false
			w.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodDelete && request.URL.Path == "/lbaas/pools/pool-1/members/member-1":
			memberExists = false
			w.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodDelete && request.URL.Path == "/lbaas/pools/pool-1":
			poolExists = false
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.RequestURI())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})
	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(loadBalancerHandler),
		ProjectID:    "project-a",
	}, ProviderConfig{OperationTimeout: time.Hour, PollInterval: time.Hour})
	ready := false
	for attempt := 0; attempt < 10; attempt++ {
		before := len(requests.mutationPaths())
		outcome, err := provider.DeleteRoute(context.Background(), identity.value)
		if err != nil {
			t.Fatalf("DeleteRoute() attempt %d error = %v", attempt+1, err)
		}
		after := len(requests.mutationPaths())
		if outcome.State == cloud.OutcomeReady {
			if after != before {
				t.Fatalf("DeleteRoute() ready attempt made %d mutations", after-before)
			}
			ready = true
			break
		}
		if after-before != 1 {
			t.Fatalf("DeleteRoute() attempt %d mutations = %d, want 1", attempt+1, after-before)
		}
	}
	if !ready {
		t.Fatal("DeleteRoute() did not converge")
	}
	want := []string{
		"/lbaas/l7policies/policy-1",
		"/lbaas/l7policies/policy-1/rules/rule-a",
		"/lbaas/l7policies/policy-1/rules/rule-z",
		"/lbaas/l7policies/policy-1",
		"/lbaas/healthmonitors/monitor-1",
		"/lbaas/pools/pool-1/members/member-1",
		"/lbaas/pools/pool-1",
	}
	if got := requests.mutationPaths(); !equalGatewayTransitionPaths(got, want) {
		t.Fatalf("mutating requests = %v, want %v", got, want)
	}
}

func routeTransitionSpec(identity Identity) cloud.RouteSpec {
	return cloud.RouteSpec{
		Identity: identity.value,
		Gateway: cloud.GatewayState{
			LoadBalancerID: "load-balancer-1",
			VIPPortID:      "vip-port",
			VIPAddress:     "192.0.2.10",
			ListenerID:     "listener-1",
		},
		MemberSubnetID: "member-subnet",
		HealthPath:     "/healthz",
		Hostname:       "api.example.com",
		PathType:       cloud.PathMatchExact,
		PathValue:      "/api",
		Members:        []cloud.Member{{Address: "10.0.0.10", Port: 30080}},
	}
}

func routeTransitionEmptyFloatingIPHandler(t *testing.T, requests *gatewayTransitionRequests) http.Handler {
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
