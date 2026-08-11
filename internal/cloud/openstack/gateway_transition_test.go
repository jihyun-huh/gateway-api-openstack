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
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

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

	outcome, err := provider.DeleteGateway(context.Background(), identity.value)
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

	outcome, err := provider.DeleteGateway(context.Background(), identity.value)
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
		Identity:     identity.value,
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
		"tags":                tags,
	}
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
