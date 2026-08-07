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
	"fmt"
	"net/http"
	"net/netip"
	"sort"
	"strings"

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/l7policies"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/monitors"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

type desiredPolicy struct {
	role        string
	position    int32
	compareType l7policies.CompareType
	path        string
}

// EnsureRoute creates or converges the pool, NodePort members, health monitor,
// and L7 policies owned by one HTTPRoute.
func (p *Provider) EnsureRoute(ctx context.Context, spec cloud.RouteSpec) (cloud.RouteState, error) {
	identity, err := p.identity(spec.Identity)
	if err != nil {
		return cloud.RouteState{}, err
	}
	if spec.Gateway.LoadBalancerID == "" || spec.Gateway.ListenerID == "" {
		return cloud.RouteState{}, fmt.Errorf("gateway state is missing load balancer or listener ID")
	}
	observedGateway, found, err := p.GetGateway(ctx, spec.Identity)
	if err != nil {
		return cloud.RouteState{}, fmt.Errorf("verify route Gateway ownership: %w", err)
	}
	if !found || observedGateway.LoadBalancerID != spec.Gateway.LoadBalancerID || observedGateway.ListenerID != spec.Gateway.ListenerID {
		return cloud.RouteState{}, fmt.Errorf("%w: route Gateway IDs do not match the controller-owned load balancer and listener", cloud.ErrOwnershipConflict)
	}
	if len(spec.Members) == 0 {
		return cloud.RouteState{}, fmt.Errorf("at least one ready backend member is required")
	}
	if strings.TrimSpace(spec.MemberSubnetID) == "" {
		return cloud.RouteState{}, fmt.Errorf("member subnet ID must not be empty")
	}
	if spec.PathType != cloud.PathMatchExact && spec.PathType != cloud.PathMatchPrefix {
		return cloud.RouteState{}, fmt.Errorf("unsupported path match type %q", spec.PathType)
	}
	if err := p.validateRoutePolicyGraph(ctx, identity, spec.Gateway.ListenerID); err != nil {
		return cloud.RouteState{}, err
	}

	pool, err := p.ensurePool(ctx, identity, spec)
	if err != nil {
		return cloud.RouteState{}, err
	}
	state := cloud.RouteState{PoolID: pool.ID}
	state.MemberIDs, err = p.syncMembers(ctx, identity, spec, pool.ID)
	if err != nil {
		return cloud.RouteState{}, err
	}
	monitor, err := p.ensureMonitor(ctx, identity, spec, pool.ID)
	if err != nil {
		return cloud.RouteState{}, err
	}
	state.MonitorID = monitor.ID
	desiredPolicies := policiesFor(spec)
	if err := p.deleteObsoletePolicies(ctx, identity, spec, desiredPolicies); err != nil {
		return cloud.RouteState{}, err
	}
	for _, desired := range desiredPolicies {
		policy, ruleIDs, ensureErr := p.ensurePolicy(ctx, identity, spec, pool.ID, desired)
		if ensureErr != nil {
			return cloud.RouteState{}, ensureErr
		}
		state.L7PolicyIDs = append(state.L7PolicyIDs, policy.ID)
		state.L7RuleIDs = append(state.L7RuleIDs, ruleIDs...)
	}
	return state, nil
}

// validateRoutePolicyGraph runs before any route mutation. Octavia L7 policy
// positions are listener-wide and first-match ordered, so inserting our fixed
// Phase 1 positions would implicitly reorder a foreign policy even if we never
// called Update on that policy directly.
func (p *Provider) validateRoutePolicyGraph(ctx context.Context, identity Identity, listenerID string) error {
	pages, err := l7policies.List(p.clients.LoadBalancer, l7policies.ListOpts{ListenerID: listenerID}).AllPages(ctx)
	if err != nil {
		return fmt.Errorf("list L7 policies before route mutation: %w", err)
	}
	policies, err := l7policies.ExtractL7Policies(pages)
	if err != nil {
		return fmt.Errorf("extract L7 policies before route mutation: %w", err)
	}
	for _, policy := range policies {
		if !matchesAnyRouteRole(identity, policy.Tags, rolePolicyExact, rolePolicyPrefix) {
			return fmt.Errorf("%w: listener %s contains L7 policy %s not owned by the selected HTTPRoute", cloud.ErrOwnershipConflict, listenerID, policy.ID)
		}
		rulePages, err := l7policies.ListRules(p.clients.LoadBalancer, policy.ID, l7policies.ListRulesOpts{}).AllPages(ctx)
		if err != nil {
			return fmt.Errorf("list L7 rules beneath policy %s before route mutation: %w", policy.ID, err)
		}
		rules, err := l7policies.ExtractRules(rulePages)
		if err != nil {
			return fmt.Errorf("extract L7 rules beneath policy %s before route mutation: %w", policy.ID, err)
		}
		for _, rule := range rules {
			if !matchesAnyRouteRole(identity, rule.Tags, roleRulePath, roleRuleHost) {
				return fmt.Errorf("%w: policy %s contains L7 rule %s not owned by the selected HTTPRoute", cloud.ErrOwnershipConflict, policy.ID, rule.ID)
			}
		}
	}
	return nil
}

func matchesAnyRouteRole(identity Identity, tags []string, roles ...string) bool {
	for _, role := range roles {
		if identity.MatchesRoute(tags, role) {
			return true
		}
	}
	return false
}

func (p *Provider) deleteObsoletePolicies(ctx context.Context, identity Identity, spec cloud.RouteSpec, desired []desiredPolicy) error {
	wanted := make(map[string]struct{}, len(desired))
	for _, policy := range desired {
		wanted[policy.role] = struct{}{}
	}
	pages, err := l7policies.List(p.clients.LoadBalancer, l7policies.ListOpts{ListenerID: spec.Gateway.ListenerID}).AllPages(ctx)
	if err != nil {
		return fmt.Errorf("list L7 policies before convergence: %w", err)
	}
	items, err := l7policies.ExtractL7Policies(pages)
	if err != nil {
		return fmt.Errorf("extract L7 policies before convergence: %w", err)
	}
	for _, policy := range items {
		role := ""
		for _, candidate := range []string{rolePolicyExact, rolePolicyPrefix} {
			if identity.MatchesRoute(policy.Tags, candidate) {
				role = candidate
				break
			} else if identity.MatchesRouteDiscovery(policy.Tags, candidate) {
				return fmt.Errorf("%w: L7 policy %s has an incomplete or stale identity", cloud.ErrOwnershipConflict, policy.ID)
			}
		}
		if role == "" {
			return fmt.Errorf("%w: listener %s contains L7 policy %s not owned by the selected HTTPRoute", cloud.ErrOwnershipConflict, spec.Gateway.ListenerID, policy.ID)
		}
		if _, keep := wanted[role]; keep {
			continue
		}
		rulePages, err := l7policies.ListRules(p.clients.LoadBalancer, policy.ID, l7policies.ListRulesOpts{}).AllPages(ctx)
		if err != nil {
			return fmt.Errorf("list obsolete policy rules: %w", err)
		}
		rules, err := l7policies.ExtractRules(rulePages)
		if err != nil {
			return fmt.Errorf("extract obsolete policy rules: %w", err)
		}
		for _, rule := range rules {
			if !identity.MatchesRoute(rule.Tags, roleRulePath) && !identity.MatchesRoute(rule.Tags, roleRuleHost) {
				return fmt.Errorf("%w: obsolete policy %s contains unmanaged rule %s", cloud.ErrOwnershipConflict, policy.ID, rule.ID)
			}
			if err := l7policies.DeleteRule(ctx, p.clients.LoadBalancer, policy.ID, rule.ID).ExtractErr(); err != nil && !isNotFound(err) {
				return fmt.Errorf("delete obsolete L7 rule %s: %w", rule.ID, err)
			}
			if _, err := waitLoadBalancerActive(ctx, p.clients.LoadBalancer, spec.Gateway.LoadBalancerID, p.operationTimeout, p.pollInterval); err != nil {
				return err
			}
		}
		if err := l7policies.Delete(ctx, p.clients.LoadBalancer, policy.ID).ExtractErr(); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete obsolete L7 policy %s: %w", policy.ID, err)
		}
		if _, err := waitLoadBalancerActive(ctx, p.clients.LoadBalancer, spec.Gateway.LoadBalancerID, p.operationTimeout, p.pollInterval); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) ensurePool(ctx context.Context, identity Identity, spec cloud.RouteSpec) (*pools.Pool, error) {
	tags, err := identity.RouteTags(rolePool)
	if err != nil {
		return nil, err
	}
	discoveryTags, err := identity.RouteDiscoveryTags(rolePool)
	if err != nil {
		return nil, err
	}
	pages, err := pools.List(p.clients.LoadBalancer, pools.ListOpts{
		LoadbalancerID: spec.Gateway.LoadBalancerID,
		Tags:           discoveryTags,
	}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list route pools: %w", err)
	}
	items, err := pools.ExtractPools(pages)
	if err != nil {
		return nil, fmt.Errorf("extract route pools: %w", err)
	}
	if len(items) > 1 {
		return nil, fmt.Errorf("%w: found %d pools for one HTTPRoute", cloud.ErrOwnershipConflict, len(items))
	}
	if len(items) == 1 {
		item := items[0]
		if !identity.MatchesRoute(item.Tags, rolePool) || item.Protocol != string(pools.ProtocolHTTP) || item.LBMethod != string(pools.LBMethodRoundRobin) {
			return nil, fmt.Errorf("%w: pool %s does not match the Phase 1 contract", cloud.ErrOwnershipConflict, item.ID)
		}
		return &item, nil
	}
	created, err := pools.Create(ctx, p.clients.LoadBalancer, pools.CreateOpts{
		LBMethod:       pools.LBMethodRoundRobin,
		Protocol:       pools.ProtocolHTTP,
		LoadbalancerID: spec.Gateway.LoadBalancerID,
		Name:           resourceName(spec.Identity, rolePool),
		Description:    identity.Description(rolePool),
		AdminStateUp:   boolPointer(true),
		Tags:           tags,
	}).Extract()
	if err != nil {
		return nil, fmt.Errorf("create route pool: %w", err)
	}
	if _, err := waitLoadBalancerActive(ctx, p.clients.LoadBalancer, spec.Gateway.LoadBalancerID, p.operationTimeout, p.pollInterval); err != nil {
		return nil, err
	}
	return created, nil
}

func (p *Provider) syncMembers(ctx context.Context, identity Identity, spec cloud.RouteSpec, poolID string) ([]string, error) {
	tags, err := identity.RouteTags(roleMember)
	if err != nil {
		return nil, err
	}
	pages, err := pools.ListMembers(p.clients.LoadBalancer, poolID, pools.ListMembersOpts{}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pool members: %w", err)
	}
	existing, err := pools.ExtractMembers(pages)
	if err != nil {
		return nil, fmt.Errorf("extract pool members: %w", err)
	}
	desired := make(map[string]cloud.Member, len(spec.Members))
	for _, member := range spec.Members {
		if member.Port < 1 || member.Port > 65535 {
			return nil, fmt.Errorf("member %s has invalid port %d", member.Address, member.Port)
		}
		if _, err := netip.ParseAddr(member.Address); err != nil {
			return nil, fmt.Errorf("member address %q is not a valid IP address: %w", member.Address, err)
		}
		desired[memberKey(member.Address, member.Port)] = member
	}
	ids := make([]string, 0, len(desired))
	for _, member := range existing {
		if !identity.MatchesRoute(member.Tags, roleMember) {
			return nil, fmt.Errorf("%w: pool %s contains unmanaged member %s", cloud.ErrOwnershipConflict, poolID, member.ID)
		}
		key := memberKey(member.Address, member.ProtocolPort)
		if _, wanted := desired[key]; wanted {
			if member.SubnetID != spec.MemberSubnetID {
				return nil, fmt.Errorf("%w: member %s belongs to subnet %s instead of %s", cloud.ErrOwnershipConflict, member.ID, member.SubnetID, spec.MemberSubnetID)
			}
			ids = append(ids, member.ID)
			delete(desired, key)
			continue
		}
		if err := pools.DeleteMember(ctx, p.clients.LoadBalancer, poolID, member.ID).ExtractErr(); err != nil && !isNotFound(err) {
			return nil, fmt.Errorf("delete stale member %s: %w", member.ID, err)
		}
		if _, err := waitLoadBalancerActive(ctx, p.clients.LoadBalancer, spec.Gateway.LoadBalancerID, p.operationTimeout, p.pollInterval); err != nil {
			return nil, err
		}
	}
	keys := make([]string, 0, len(desired))
	for key := range desired {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		member := desired[key]
		created, err := pools.CreateMember(ctx, p.clients.LoadBalancer, poolID, pools.CreateMemberOpts{
			Address:      member.Address,
			ProtocolPort: member.Port,
			Name:         resourceName(spec.Identity, roleMember),
			SubnetID:     spec.MemberSubnetID,
			AdminStateUp: boolPointer(true),
			Tags:         tags,
		}).Extract()
		if err != nil {
			return nil, fmt.Errorf("create member %s: %w", key, err)
		}
		ids = append(ids, created.ID)
		if _, err := waitLoadBalancerActive(ctx, p.clients.LoadBalancer, spec.Gateway.LoadBalancerID, p.operationTimeout, p.pollInterval); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func (p *Provider) ensureMonitor(ctx context.Context, identity Identity, spec cloud.RouteSpec, poolID string) (*monitors.Monitor, error) {
	tags, err := identity.RouteTags(roleMonitor)
	if err != nil {
		return nil, err
	}
	pages, err := monitors.List(p.clients.LoadBalancer, monitors.ListOpts{PoolID: poolID}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list health monitors: %w", err)
	}
	items, err := monitors.ExtractMonitors(pages)
	if err != nil {
		return nil, fmt.Errorf("extract health monitors: %w", err)
	}
	if len(items) > 1 {
		return nil, fmt.Errorf("%w: pool %s has multiple health monitors", cloud.ErrOwnershipConflict, poolID)
	}
	healthPath := spec.HealthPath
	if healthPath == "" {
		healthPath = "/"
	}
	if len(items) == 1 {
		item := items[0]
		if !identity.MatchesRoute(item.Tags, roleMonitor) || item.Type != monitors.TypeHTTP || item.URLPath != healthPath {
			return nil, fmt.Errorf("%w: health monitor %s does not match the desired configuration", cloud.ErrOwnershipConflict, item.ID)
		}
		return &item, nil
	}
	created, err := monitors.Create(ctx, p.clients.LoadBalancer, monitors.CreateOpts{
		PoolID:         poolID,
		Type:           monitors.TypeHTTP,
		Delay:          10,
		Timeout:        5,
		MaxRetries:     3,
		MaxRetriesDown: 3,
		URLPath:        healthPath,
		HTTPMethod:     http.MethodGet,
		ExpectedCodes:  "200-399",
		Name:           resourceName(spec.Identity, roleMonitor),
		AdminStateUp:   boolPointer(true),
		Tags:           tags,
	}).Extract()
	if err != nil {
		return nil, fmt.Errorf("create health monitor: %w", err)
	}
	if _, err := waitLoadBalancerActive(ctx, p.clients.LoadBalancer, spec.Gateway.LoadBalancerID, p.operationTimeout, p.pollInterval); err != nil {
		return nil, err
	}
	return created, nil
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

func (p *Provider) ensurePolicy(ctx context.Context, identity Identity, spec cloud.RouteSpec, poolID string, desired desiredPolicy) (*l7policies.L7Policy, []string, error) {
	tags, err := identity.RouteTags(desired.role)
	if err != nil {
		return nil, nil, err
	}
	pages, err := l7policies.List(p.clients.LoadBalancer, l7policies.ListOpts{ListenerID: spec.Gateway.ListenerID}).AllPages(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list L7 policies: %w", err)
	}
	items, err := l7policies.ExtractL7Policies(pages)
	if err != nil {
		return nil, nil, fmt.Errorf("extract L7 policies: %w", err)
	}
	var owned []l7policies.L7Policy
	for _, item := range items {
		if identity.MatchesRoute(item.Tags, desired.role) {
			owned = append(owned, item)
		} else if identity.MatchesRouteDiscovery(item.Tags, desired.role) {
			return nil, nil, fmt.Errorf("%w: L7 policy %s has an incomplete or stale identity", cloud.ErrOwnershipConflict, item.ID)
		} else {
			return nil, nil, fmt.Errorf("%w: listener %s contains L7 policy %s not owned by the selected HTTPRoute", cloud.ErrOwnershipConflict, spec.Gateway.ListenerID, item.ID)
		}
	}
	if len(owned) > 1 {
		return nil, nil, fmt.Errorf("%w: duplicate %s policies", cloud.ErrOwnershipConflict, desired.role)
	}
	var policy *l7policies.L7Policy
	if len(owned) == 1 {
		item := owned[0]
		if item.Action != string(l7policies.ActionRedirectToPool) || item.RedirectPoolID != poolID {
			return nil, nil, fmt.Errorf("%w: L7 policy %s has an unexpected action", cloud.ErrOwnershipConflict, item.ID)
		}
		if item.Position != desired.position {
			updated, updateErr := l7policies.Update(ctx, p.clients.LoadBalancer, item.ID, l7policies.UpdateOpts{Position: desired.position}).Extract()
			if updateErr != nil {
				return nil, nil, fmt.Errorf("update L7 policy %s position: %w", item.ID, updateErr)
			}
			item = *updated
			if _, waitErr := waitLoadBalancerActive(ctx, p.clients.LoadBalancer, spec.Gateway.LoadBalancerID, p.operationTimeout, p.pollInterval); waitErr != nil {
				return nil, nil, waitErr
			}
		}
		policy = &item
	} else {
		policy, err = l7policies.Create(ctx, p.clients.LoadBalancer, l7policies.CreateOpts{
			Name:           resourceName(spec.Identity, desired.role),
			ListenerID:     spec.Gateway.ListenerID,
			Action:         l7policies.ActionRedirectToPool,
			Position:       desired.position,
			Description:    identity.Description(desired.role),
			RedirectPoolID: poolID,
			AdminStateUp:   boolPointer(true),
			Tags:           tags,
		}).Extract()
		if err != nil {
			return nil, nil, fmt.Errorf("create %s policy: %w", desired.role, err)
		}
		if _, err := waitLoadBalancerActive(ctx, p.clients.LoadBalancer, spec.Gateway.LoadBalancerID, p.operationTimeout, p.pollInterval); err != nil {
			return nil, nil, err
		}
	}
	ruleIDs, err := p.ensurePolicyRules(ctx, identity, spec, policy.ID, desired)
	return policy, ruleIDs, err
}

func (p *Provider) ensurePolicyRules(ctx context.Context, identity Identity, spec cloud.RouteSpec, policyID string, desired desiredPolicy) ([]string, error) {
	type ruleSpec struct {
		role        string
		ruleType    l7policies.RuleType
		compareType l7policies.CompareType
		value       string
	}
	desiredRules := []ruleSpec{{role: roleRulePath, ruleType: l7policies.TypePath, compareType: desired.compareType, value: desired.path}}
	if spec.Hostname != "" {
		desiredRules = append(desiredRules, ruleSpec{role: roleRuleHost, ruleType: l7policies.TypeHostName, compareType: l7policies.CompareTypeEqual, value: spec.Hostname})
	}
	pages, err := l7policies.ListRules(p.clients.LoadBalancer, policyID, l7policies.ListRulesOpts{}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list rules for policy %s: %w", policyID, err)
	}
	existing, err := l7policies.ExtractRules(pages)
	if err != nil {
		return nil, fmt.Errorf("extract rules for policy %s: %w", policyID, err)
	}
	ids := make([]string, 0, len(desiredRules))
	wantedIDs := make(map[string]struct{}, len(desiredRules))
	for _, wanted := range desiredRules {
		var matches []l7policies.Rule
		for _, item := range existing {
			if identity.MatchesRoute(item.Tags, wanted.role) {
				matches = append(matches, item)
			} else if identity.MatchesRouteDiscovery(item.Tags, wanted.role) {
				return nil, fmt.Errorf("%w: rule %s has an incomplete or stale identity", cloud.ErrOwnershipConflict, item.ID)
			}
		}
		if len(matches) > 1 {
			return nil, fmt.Errorf("%w: policy %s has duplicate %s rules", cloud.ErrOwnershipConflict, policyID, wanted.role)
		}
		if len(matches) == 1 {
			item := matches[0]
			if item.RuleType != string(wanted.ruleType) || item.CompareType != string(wanted.compareType) || item.Value != wanted.value {
				tags, tagErr := identity.RouteTags(wanted.role)
				if tagErr != nil {
					return nil, tagErr
				}
				updated, updateErr := l7policies.UpdateRule(ctx, p.clients.LoadBalancer, policyID, item.ID, l7policies.UpdateRuleOpts{
					RuleType:    wanted.ruleType,
					CompareType: wanted.compareType,
					Value:       wanted.value,
					Tags:        &tags,
				}).Extract()
				if updateErr != nil {
					return nil, fmt.Errorf("update %s rule %s: %w", wanted.role, item.ID, updateErr)
				}
				item = *updated
				if _, waitErr := waitLoadBalancerActive(ctx, p.clients.LoadBalancer, spec.Gateway.LoadBalancerID, p.operationTimeout, p.pollInterval); waitErr != nil {
					return nil, waitErr
				}
			}
			ids = append(ids, item.ID)
			wantedIDs[item.ID] = struct{}{}
			continue
		}
		tags, err := identity.RouteTags(wanted.role)
		if err != nil {
			return nil, err
		}
		created, err := l7policies.CreateRule(ctx, p.clients.LoadBalancer, policyID, l7policies.CreateRuleOpts{
			RuleType:     wanted.ruleType,
			CompareType:  wanted.compareType,
			Value:        wanted.value,
			AdminStateUp: boolPointer(true),
			Tags:         tags,
		}).Extract()
		if err != nil {
			return nil, fmt.Errorf("create %s rule: %w", wanted.role, err)
		}
		ids = append(ids, created.ID)
		wantedIDs[created.ID] = struct{}{}
		if _, err := waitLoadBalancerActive(ctx, p.clients.LoadBalancer, spec.Gateway.LoadBalancerID, p.operationTimeout, p.pollInterval); err != nil {
			return nil, err
		}
	}
	for _, item := range existing {
		if _, keep := wantedIDs[item.ID]; keep {
			continue
		}
		if !identity.MatchesRoute(item.Tags, roleRulePath) && !identity.MatchesRoute(item.Tags, roleRuleHost) {
			return nil, fmt.Errorf("%w: policy %s contains unmanaged rule %s", cloud.ErrOwnershipConflict, policyID, item.ID)
		}
		if err := l7policies.DeleteRule(ctx, p.clients.LoadBalancer, policyID, item.ID).ExtractErr(); err != nil && !isNotFound(err) {
			return nil, fmt.Errorf("delete stale rule %s: %w", item.ID, err)
		}
		if _, err := waitLoadBalancerActive(ctx, p.clients.LoadBalancer, spec.Gateway.LoadBalancerID, p.operationTimeout, p.pollInterval); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func memberKey(address string, port int) string { return fmt.Sprintf("%s:%d", address, port) }
