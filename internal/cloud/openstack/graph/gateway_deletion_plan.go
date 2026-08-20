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
	"fmt"
	"sort"

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/l7policies"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/monitors"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

type gatewayDeletionResource string

const (
	gatewayDeletionL7Rule   gatewayDeletionResource = "L7 rule"
	gatewayDeletionL7Policy gatewayDeletionResource = "L7 policy"
	gatewayDeletionListener gatewayDeletionResource = "listener"
	gatewayDeletionMonitor  gatewayDeletionResource = "health monitor"
	gatewayDeletionMember   gatewayDeletionResource = "member"
	gatewayDeletionPool     gatewayDeletionResource = "pool"
)

type gatewayDeletionStep struct {
	resource   gatewayDeletionResource
	resourceID string
	parentID   string
}

type gatewayListenerDeletionPlan []gatewayDeletionStep
type gatewayRouteDeletionPlan []gatewayDeletionStep
type gatewayPoolDeletionPlan []gatewayDeletionStep

// gatewayDeletionPlan separates the load balancer graph into dependency
// subplans. No step may run until all three subplans have been validated.
type gatewayDeletionPlan struct {
	listeners gatewayListenerDeletionPlan
	routes    gatewayRouteDeletionPlan
	pools     gatewayPoolDeletionPlan
}

func (p gatewayDeletionPlan) hasRouteResources() bool {
	return len(p.routes) != 0 || len(p.pools) != 0
}

func (p gatewayDeletionPlan) firstStep() (gatewayDeletionStep, bool) {
	steps := p.orderedSteps()
	if len(steps) == 0 {
		return gatewayDeletionStep{}, false
	}
	return steps[0], true
}

// orderedSteps flattens the validated subplans in reverse dependency order.
func (p gatewayDeletionPlan) orderedSteps() []gatewayDeletionStep {
	steps := make([]gatewayDeletionStep, 0, len(p.routes)+len(p.pools)+len(p.listeners))
	steps = append(steps, p.routes...)
	steps = append(steps, p.pools...)
	steps = append(steps, p.listeners...)
	return steps
}

// buildGatewayDeletionPlan validates the complete load balancer graph before
// returning any work. Callers must not execute a deletion step when this
// method returns an error.
func (p *Provider) buildGatewayDeletionPlan(ctx context.Context, identity Identity, loadBalancerID string) (gatewayDeletionPlan, error) {
	listenerPlan, routePlan, err := p.buildGatewayListenerDeletionPlans(ctx, identity, loadBalancerID)
	if err != nil {
		return gatewayDeletionPlan{}, err
	}
	poolPlan, err := p.buildGatewayPoolDeletionPlan(ctx, identity, loadBalancerID)
	if err != nil {
		return gatewayDeletionPlan{}, err
	}
	return gatewayDeletionPlan{
		listeners: listenerPlan,
		routes:    routePlan,
		pools:     poolPlan,
	}, nil
}

// buildGatewayListenerDeletionPlans observes listeners and their policy and
// rule subtrees in listener order. It returns listener deletion separately so
// the aggregate plan can place listeners after every route resource.
func (p *Provider) buildGatewayListenerDeletionPlans(
	ctx context.Context,
	identity Identity,
	loadBalancerID string,
) (gatewayListenerDeletionPlan, gatewayRouteDeletionPlan, error) {
	listenerList, err := p.octavia.ListListeners(ctx, listeners.ListOpts{
		LoadbalancerID: loadBalancerID,
		ProjectID:      p.projectID,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list listeners before Gateway deletion: %w", err)
	}
	sort.Slice(listenerList, func(i, j int) bool { return listenerList[i].ID < listenerList[j].ID })

	listenerPlan := make(gatewayListenerDeletionPlan, 0, len(listenerList))
	var routePlan gatewayRouteDeletionPlan
	for _, listener := range listenerList {
		if err := p.validateOptionalProject("listener", listener.ID, listener.ProjectID); err != nil {
			return nil, nil, err
		}
		if !identity.MatchesGateway(listener.Tags, roleListener) {
			return nil, nil, fmt.Errorf("%w: listener %s", cloud.ErrOwnershipConflict, listener.ID)
		}
		for _, parent := range listener.Loadbalancers {
			if parent.ID != loadBalancerID {
				return nil, nil, fmt.Errorf("%w: listener %s reports load balancer %s instead of %s", cloud.ErrOwnershipConflict, listener.ID, parent.ID, loadBalancerID)
			}
		}

		listenerRoutePlan, err := p.buildGatewayRouteDeletionPlan(ctx, identity, listener.ID)
		if err != nil {
			return nil, nil, err
		}
		routePlan = append(routePlan, listenerRoutePlan...)
		listenerPlan = append(listenerPlan, gatewayDeletionStep{
			resource:   gatewayDeletionListener,
			resourceID: listener.ID,
		})
	}
	return listenerPlan, routePlan, nil
}

func (p *Provider) buildGatewayRouteDeletionPlan(ctx context.Context, identity Identity, listenerID string) (gatewayRouteDeletionPlan, error) {
	policyList, err := p.octavia.ListPolicies(ctx, l7policies.ListOpts{
		ListenerID: listenerID,
		ProjectID:  p.projectID,
	})
	if err != nil {
		return nil, fmt.Errorf("list L7 policies beneath listener %s before Gateway deletion: %w", listenerID, err)
	}
	sort.Slice(policyList, func(i, j int) bool {
		if policyList[i].Position == policyList[j].Position {
			return policyList[i].ID < policyList[j].ID
		}
		return policyList[i].Position < policyList[j].Position
	})

	var plan gatewayRouteDeletionPlan
	for _, policy := range policyList {
		if err := p.validateOptionalProject("L7 policy", policy.ID, policy.ProjectID); err != nil {
			return nil, err
		}
		if !matchesAnyGatewayRole(identity, policy.Tags, rolePolicyExact, rolePolicyPrefix) {
			return nil, fmt.Errorf("%w: L7 policy %s", cloud.ErrOwnershipConflict, policy.ID)
		}
		if policy.ListenerID != "" && policy.ListenerID != listenerID {
			return nil, fmt.Errorf("%w: L7 policy %s reports listener %s instead of %s", cloud.ErrOwnershipConflict, policy.ID, policy.ListenerID, listenerID)
		}

		ruleList, err := p.octavia.ListRules(ctx, policy.ID, l7policies.ListRulesOpts{ProjectID: p.projectID})
		if err != nil {
			return nil, fmt.Errorf("list L7 rules beneath policy %s before Gateway deletion: %w", policy.ID, err)
		}
		sort.Slice(ruleList, func(i, j int) bool { return ruleList[i].ID < ruleList[j].ID })
		for _, rule := range ruleList {
			if err := p.validateOptionalProject("L7 rule", rule.ID, rule.ProjectID); err != nil {
				return nil, err
			}
			if !matchesAnyGatewayRole(identity, rule.Tags, roleRulePath, roleRuleHost) {
				return nil, fmt.Errorf("%w: L7 rule %s", cloud.ErrOwnershipConflict, rule.ID)
			}
			plan = append(plan, gatewayDeletionStep{
				resource:   gatewayDeletionL7Rule,
				resourceID: rule.ID,
				parentID:   policy.ID,
			})
		}
		plan = append(plan, gatewayDeletionStep{
			resource:   gatewayDeletionL7Policy,
			resourceID: policy.ID,
		})
	}
	return plan, nil
}

func (p *Provider) buildGatewayPoolDeletionPlan(ctx context.Context, identity Identity, loadBalancerID string) (gatewayPoolDeletionPlan, error) {
	poolList, err := p.octavia.ListPools(ctx, pools.ListOpts{
		LoadbalancerID: loadBalancerID,
		ProjectID:      p.projectID,
	})
	if err != nil {
		return nil, fmt.Errorf("list pools before Gateway deletion: %w", err)
	}
	sort.Slice(poolList, func(i, j int) bool { return poolList[i].ID < poolList[j].ID })

	var plan gatewayPoolDeletionPlan
	for _, pool := range poolList {
		if err := p.validateGatewayDeletionPool(identity, pool, loadBalancerID); err != nil {
			return nil, err
		}
		subtree, err := p.buildGatewayPoolSubtreeDeletionPlan(ctx, identity, pool.ID)
		if err != nil {
			return nil, err
		}
		plan = append(plan, subtree...)
	}
	return plan, nil
}

func (p *Provider) validateGatewayDeletionPool(identity Identity, pool pools.Pool, loadBalancerID string) error {
	if err := p.validateOptionalProject("pool", pool.ID, pool.ProjectID); err != nil {
		return err
	}
	if !identity.MatchesGateway(pool.Tags, rolePool) {
		return fmt.Errorf("%w: pool %s", cloud.ErrOwnershipConflict, pool.ID)
	}
	for _, parent := range pool.Loadbalancers {
		if parent.ID != loadBalancerID {
			return fmt.Errorf("%w: pool %s reports load balancer %s instead of %s", cloud.ErrOwnershipConflict, pool.ID, parent.ID, loadBalancerID)
		}
	}
	return nil
}

func (p *Provider) buildGatewayPoolSubtreeDeletionPlan(ctx context.Context, identity Identity, poolID string) (gatewayPoolDeletionPlan, error) {
	monitorPlan, err := p.buildGatewayMonitorDeletionPlan(ctx, identity, poolID)
	if err != nil {
		return nil, err
	}
	memberPlan, err := p.buildGatewayMemberDeletionPlan(ctx, identity, poolID)
	if err != nil {
		return nil, err
	}
	plan := make(gatewayPoolDeletionPlan, 0, len(monitorPlan)+len(memberPlan)+1)
	plan = append(plan, monitorPlan...)
	plan = append(plan, memberPlan...)
	plan = append(plan, gatewayDeletionStep{
		resource:   gatewayDeletionPool,
		resourceID: poolID,
	})
	return plan, nil
}

func (p *Provider) buildGatewayMonitorDeletionPlan(ctx context.Context, identity Identity, poolID string) (gatewayPoolDeletionPlan, error) {
	monitorList, err := p.octavia.ListMonitors(ctx, monitors.ListOpts{PoolID: poolID, ProjectID: p.projectID})
	if err != nil {
		return nil, fmt.Errorf("list health monitors beneath pool %s before Gateway deletion: %w", poolID, err)
	}
	sort.Slice(monitorList, func(i, j int) bool { return monitorList[i].ID < monitorList[j].ID })

	plan := make(gatewayPoolDeletionPlan, 0, len(monitorList))
	for _, monitor := range monitorList {
		if err := p.validateOptionalProject("health monitor", monitor.ID, monitor.ProjectID); err != nil {
			return nil, err
		}
		if !identity.MatchesGateway(monitor.Tags, roleMonitor) {
			return nil, fmt.Errorf("%w: monitor %s", cloud.ErrOwnershipConflict, monitor.ID)
		}
		for _, parent := range monitor.Pools {
			if parent.ID != poolID {
				return nil, fmt.Errorf("%w: health monitor %s reports pool %s instead of %s", cloud.ErrOwnershipConflict, monitor.ID, parent.ID, poolID)
			}
		}
		plan = append(plan, gatewayDeletionStep{
			resource:   gatewayDeletionMonitor,
			resourceID: monitor.ID,
			parentID:   poolID,
		})
	}
	return plan, nil
}

func (p *Provider) buildGatewayMemberDeletionPlan(ctx context.Context, identity Identity, poolID string) (gatewayPoolDeletionPlan, error) {
	memberList, err := p.octavia.ListMembers(ctx, poolID, pools.ListMembersOpts{ProjectID: p.projectID})
	if err != nil {
		return nil, fmt.Errorf("list members beneath pool %s before Gateway deletion: %w", poolID, err)
	}
	sort.Slice(memberList, func(i, j int) bool {
		if memberList[i].Address == memberList[j].Address {
			if memberList[i].ProtocolPort == memberList[j].ProtocolPort {
				return memberList[i].ID < memberList[j].ID
			}
			return memberList[i].ProtocolPort < memberList[j].ProtocolPort
		}
		return memberList[i].Address < memberList[j].Address
	})

	plan := make(gatewayPoolDeletionPlan, 0, len(memberList))
	for _, member := range memberList {
		if err := p.validateOptionalProject("member", member.ID, member.ProjectID); err != nil {
			return nil, err
		}
		if !identity.MatchesGateway(member.Tags, roleMember) {
			return nil, fmt.Errorf("%w: member %s", cloud.ErrOwnershipConflict, member.ID)
		}
		if member.PoolID != "" && member.PoolID != poolID {
			return nil, fmt.Errorf("%w: member %s reports pool %s instead of %s", cloud.ErrOwnershipConflict, member.ID, member.PoolID, poolID)
		}
		plan = append(plan, gatewayDeletionStep{
			resource:   gatewayDeletionMember,
			resourceID: member.ID,
			parentID:   poolID,
		})
	}
	return plan, nil
}

func matchesAnyGatewayRole(identity Identity, tags []string, roles ...string) bool {
	for _, role := range roles {
		if identity.MatchesGateway(tags, role) {
			return true
		}
	}
	return false
}
