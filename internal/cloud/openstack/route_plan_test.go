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
	"net/http"
	"reflect"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/l7policies"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/monitors"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

func TestBuildRouteEnsurePlanDisablesPolicyBeforeChangingRules(t *testing.T) {
	desired, actual := convergedRoutePlanFixture()
	desired.spec.Hostname = "api.example.test"

	plan, _, err := buildRouteEnsurePlan(desired, actual)
	if err != nil {
		t.Fatalf("buildRouteEnsurePlan() error = %v", err)
	}
	if len(plan) == 0 || plan[0] != (routeMutation{kind: routeMutationDisablePolicy, id: "policy-1"}) {
		t.Fatalf("first route mutation = %#v, want policy disable", firstRouteMutation(plan))
	}
}

func TestBuildRouteEnsurePlanEnablesOnlyConvergedPolicy(t *testing.T) {
	desired, actual := convergedRoutePlanFixture()
	actual.policies[0].policy.AdminStateUp = false

	plan, _, err := buildRouteEnsurePlan(desired, actual)
	if err != nil {
		t.Fatalf("buildRouteEnsurePlan() error = %v", err)
	}
	want := []routeMutation{{kind: routeMutationEnablePolicy, id: "policy-1"}}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("route plan = %#v, want %#v", plan, want)
	}
}

func TestBuildRouteEnsurePlanReturnsDeterministicConvergedState(t *testing.T) {
	desired, actual := convergedRoutePlanFixture()

	plan, state, err := buildRouteEnsurePlan(desired, actual)
	if err != nil {
		t.Fatalf("buildRouteEnsurePlan() error = %v", err)
	}
	if len(plan) != 0 {
		t.Fatalf("converged route plan = %#v, want no mutations", plan)
	}
	want := cloud.RouteState{
		PoolID:      "pool-1",
		MemberIDs:   []string{"member-1"},
		MonitorID:   "monitor-1",
		L7PolicyIDs: []string{"policy-1"},
		L7RuleIDs:   []string{"rule-path"},
	}
	if !reflect.DeepEqual(state, want) {
		t.Fatalf("converged route state = %#v, want %#v", state, want)
	}
}

func TestBuildRouteEnsurePlanRepairsDisabledMember(t *testing.T) {
	desired, actual := convergedRoutePlanFixture()
	actual.members[0].AdminStateUp = false

	plan, _, err := buildRouteEnsurePlan(desired, actual)
	if err != nil {
		t.Fatalf("buildRouteEnsurePlan() error = %v", err)
	}
	want := []routeMutation{{
		kind:     routeMutationUpdateMember,
		id:       "member-1",
		parentID: "pool-1",
	}}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("route plan = %#v, want %#v", plan, want)
	}
}

func TestBuildRouteDeletionPlanDisablesPolicyBeforeDeletingRules(t *testing.T) {
	_, actual := convergedRoutePlanFixture()
	plan := buildRouteDeletionPlan(actual)
	if len(plan) < 2 {
		t.Fatalf("route deletion plan = %#v, want policy disable and rule deletion", plan)
	}
	if plan[0] != (routeMutation{kind: routeMutationDisablePolicy, id: "policy-1"}) {
		t.Fatalf("first route deletion = %#v, want policy disable", plan[0])
	}
	if plan[1] != (routeMutation{kind: routeMutationDeleteRule, id: "rule-path", parentID: "policy-1"}) {
		t.Fatalf("second route deletion = %#v, want path rule deletion", plan[1])
	}
}

func convergedRoutePlanFixture() (desiredRoute, observedRouteGraph) {
	policy := desiredPolicy{
		role:        rolePolicyExact,
		position:    1,
		compareType: l7policies.CompareTypeEqual,
		path:        "/api",
	}
	desired := desiredRoute{
		spec: cloud.RouteSpec{
			MemberSubnetID: "member-subnet",
			PathType:       cloud.PathMatchExact,
			PathValue:      "/api",
		},
		members:    []cloud.Member{{Address: "10.0.0.10", Port: 30080}},
		healthPath: "/healthz",
		policies:   []desiredPolicy{policy},
	}
	actual := observedRouteGraph{
		pool: &pools.Pool{
			ID:           "pool-1",
			Protocol:     string(pools.ProtocolHTTP),
			LBMethod:     string(pools.LBMethodRoundRobin),
			AdminStateUp: true,
		},
		members: []pools.Member{{
			ID: "member-1", Address: "10.0.0.10", ProtocolPort: 30080, SubnetID: "member-subnet", AdminStateUp: true,
		}},
		monitor: &monitors.Monitor{
			ID:             "monitor-1",
			Type:           monitors.TypeHTTP,
			Delay:          routeMonitorDelay,
			Timeout:        routeMonitorTimeout,
			MaxRetries:     routeMonitorMaxRetries,
			MaxRetriesDown: routeMonitorMaxRetriesDown,
			URLPath:        "/healthz",
			HTTPMethod:     http.MethodGet,
			ExpectedCodes:  routeMonitorExpectedCodes,
			AdminStateUp:   true,
		},
		policies: []observedRoutePolicy{{
			role: rolePolicyExact,
			policy: l7policies.L7Policy{
				ID: "policy-1", Action: string(l7policies.ActionRedirectToPool), RedirectPoolID: "pool-1",
				Position: 1, AdminStateUp: true,
			},
			rules: []observedRouteRule{{
				role: roleRulePath,
				rule: l7policies.Rule{
					ID: "rule-path", RuleType: string(l7policies.TypePath),
					CompareType: string(l7policies.CompareTypeEqual), Value: "/api", AdminStateUp: true,
				},
			}},
		}},
	}
	return desired, actual
}

func firstRouteMutation(plan []routeMutation) routeMutation {
	if len(plan) == 0 {
		return routeMutation{}
	}
	return plan[0]
}
