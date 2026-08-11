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
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/l7policies"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

type desiredPolicy struct {
	role        string
	position    int32
	compareType l7policies.CompareType
	path        string
}

type desiredRule struct {
	role        string
	ruleType    l7policies.RuleType
	compareType l7policies.CompareType
	value       string
}

type desiredRoute struct {
	identity   Identity
	spec       cloud.RouteSpec
	members    []cloud.Member
	healthPath string
	policies   []desiredPolicy
}

// EnsureRoute observes and validates the complete route graph, then executes
// at most one Octavia mutation. A later reconciliation observes the result.
func (p *Provider) EnsureRoute(ctx context.Context, spec cloud.RouteSpec) (result cloud.RouteResult, retErr error) {
	defer func() {
		retErr = classifyOpenStackError(retErr)
	}()

	ctx, cancel := p.operationContext(ctx)
	defer cancel()

	desired, err := p.buildDesiredRoute(spec)
	if err != nil {
		return cloud.RouteResult{}, err
	}

	gateway, found, err := p.observeRouteGateway(ctx, desired.identity, true)
	if err != nil {
		return cloud.RouteResult{}, fmt.Errorf("observe Gateway for HTTPRoute: %w", err)
	}
	if !found {
		return cloud.RouteProgressingResult("Controller-owned Octavia load balancer has not been created", p.pollInterval), nil
	}
	if gateway.outcome.State == cloud.OutcomeProgressing {
		return cloud.RouteResult{Outcome: gateway.outcome}, nil
	}
	if desired.spec.Gateway.LoadBalancerID != "" && desired.spec.Gateway.LoadBalancerID != gateway.state.LoadBalancerID ||
		desired.spec.Gateway.ListenerID != "" && desired.spec.Gateway.ListenerID != gateway.state.ListenerID {
		return cloud.RouteResult{}, fmt.Errorf("%w: route Gateway IDs do not match the controller-owned load balancer and listener", cloud.ErrOwnershipConflict)
	}
	desired.spec.Gateway = gateway.state

	actual, err := p.observeRouteGraph(ctx, desired.identity, gateway.state)
	if err != nil {
		return cloud.RouteResult{}, err
	}
	plan, state, err := buildRouteEnsurePlan(desired, actual)
	if err != nil {
		return cloud.RouteResult{}, err
	}
	if len(plan) == 0 {
		return cloud.RouteReadyResult(state), nil
	}
	if err := p.executeRouteMutation(ctx, desired, plan[0]); err != nil {
		return cloud.RouteResult{}, err
	}
	return cloud.RouteResult{
		State:   state,
		Outcome: p.progressingOutcome(plan[0].message()),
	}, nil
}

func (p *Provider) buildDesiredRoute(spec cloud.RouteSpec) (desiredRoute, error) {
	identity, err := p.identity(spec.Identity)
	if err != nil {
		return desiredRoute{}, err
	}
	invalid := func(err error) (desiredRoute, error) {
		return desiredRoute{}, cloud.NewProviderError(cloud.ErrorCategoryTerminalValidation, err)
	}
	for _, role := range []string{
		rolePool,
		roleMember,
		roleMonitor,
		rolePolicyExact,
		rolePolicyPrefix,
		roleRulePath,
		roleRuleHost,
	} {
		if _, err := identity.RouteTags(role); err != nil {
			return invalid(fmt.Errorf("build %s identity tags: %w", role, err))
		}
	}
	if len(spec.Members) == 0 {
		return invalid(errors.New("at least one ready backend member is required"))
	}
	if strings.TrimSpace(spec.MemberSubnetID) == "" {
		return invalid(errors.New("member subnet ID must not be empty"))
	}
	if spec.PathType != cloud.PathMatchExact && spec.PathType != cloud.PathMatchPrefix {
		return invalid(fmt.Errorf("unsupported path match type %q", spec.PathType))
	}

	membersByKey := make(map[string]cloud.Member, len(spec.Members))
	for _, member := range spec.Members {
		if member.Port < 1 || member.Port > 65535 {
			return invalid(fmt.Errorf("member %s has invalid port %d", member.Address, member.Port))
		}
		address, err := netip.ParseAddr(member.Address)
		if err != nil {
			return invalid(fmt.Errorf("member address %q is not a valid IP address: %w", member.Address, err))
		}
		member.Address = address.String()
		membersByKey[memberKey(member.Address, member.Port)] = member
	}
	keys := make([]string, 0, len(membersByKey))
	for key := range membersByKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	members := make([]cloud.Member, 0, len(keys))
	for _, key := range keys {
		members = append(members, membersByKey[key])
	}

	healthPath := spec.HealthPath
	if healthPath == "" {
		healthPath = "/"
	}
	if !strings.HasPrefix(healthPath, "/") {
		return invalid(fmt.Errorf("health check path %q must start with /", healthPath))
	}
	return desiredRoute{
		identity:   identity,
		spec:       spec,
		members:    members,
		healthPath: healthPath,
		policies:   policiesFor(spec),
	}, nil
}

func policiesFor(spec cloud.RouteSpec) []desiredPolicy {
	path := spec.PathValue
	if path == "" {
		path = "/"
	}
	if spec.PathType == cloud.PathMatchExact {
		return []desiredPolicy{{role: rolePolicyExact, position: 1, compareType: l7policies.CompareTypeEqual, path: path}}
	}
	path = strings.TrimRight(path, "/")
	if path == "" {
		path = "/"
	}
	if path == "/" {
		return []desiredPolicy{{role: rolePolicyPrefix, position: 1, compareType: l7policies.CompareTypeStartWith, path: "/"}}
	}
	return []desiredPolicy{
		{role: rolePolicyExact, position: 1, compareType: l7policies.CompareTypeEqual, path: path},
		{role: rolePolicyPrefix, position: 2, compareType: l7policies.CompareTypeStartWith, path: path + "/"},
	}
}

func rulesFor(policy desiredPolicy, hostname string) []desiredRule {
	rules := []desiredRule{{
		role:        roleRulePath,
		ruleType:    l7policies.TypePath,
		compareType: policy.compareType,
		value:       policy.path,
	}}
	if hostname != "" {
		rules = append(rules, desiredRule{
			role:        roleRuleHost,
			ruleType:    l7policies.TypeHostName,
			compareType: l7policies.CompareTypeEqual,
			value:       hostname,
		})
	}
	return rules
}

func memberKey(address string, port int) string {
	parsed, err := netip.ParseAddr(address)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Sprintf("%s:%d", address, port)
	}
	return netip.AddrPortFrom(parsed, uint16(port)).String()
}
