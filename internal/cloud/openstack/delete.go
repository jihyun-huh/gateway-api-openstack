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
	"sort"

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/l7policies"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/monitors"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

// DeleteRoute validates the complete route graph and removes at most one
// resource in reverse dependency order.
func (p *Provider) DeleteRoute(ctx context.Context, value cloud.Identity) (outcome cloud.Outcome, retErr error) {
	defer func() {
		retErr = classifyOpenStackError(retErr)
	}()

	ctx, cancel := p.operationContext(ctx)
	defer cancel()

	identity, err := p.identity(value)
	if err != nil {
		return cloud.Outcome{}, err
	}
	gateway, found, err := p.observeRouteGateway(ctx, identity, nil, false)
	if err != nil {
		return cloud.Outcome{}, fmt.Errorf("observe Gateway before HTTPRoute deletion: %w", err)
	}
	if !found {
		return cloud.ReadyOutcome(), nil
	}
	if gateway.outcome.State == cloud.OutcomeProgressing {
		return gateway.outcome, nil
	}
	actual, err := p.observeRouteGraph(ctx, identity, gateway.state)
	if err != nil {
		return cloud.Outcome{}, err
	}
	plan := buildRouteDeletionPlan(actual)
	if len(plan) == 0 {
		return cloud.ReadyOutcome(), nil
	}
	desired := desiredRoute{
		identity: identity,
		spec: cloud.RouteSpec{
			Identity: value,
			Gateway:  gateway.state,
		},
	}
	if err := p.executeRouteMutation(ctx, desired, plan[0]); err != nil {
		return cloud.Outcome{}, err
	}
	return p.progressingOutcome(plan[0].message()), nil
}

// DeleteGateway removes the complete controller-owned graph. Descendants are
// validated and deleted explicitly before the load balancer is deleted without
// cascade, so a resource added after validation cannot be swept up implicitly.
func (p *Provider) DeleteGateway(ctx context.Context, cloudIdentity cloud.Identity) (outcome cloud.Outcome, retErr error) {
	defer func() {
		retErr = classifyOpenStackError(retErr)
	}()

	ctx, cancel := p.operationContext(ctx)
	defer cancel()

	identity, err := p.identity(cloudIdentity)
	if err != nil {
		return cloud.Outcome{}, err
	}
	managedLoadBalancer, err := p.findGatewayLoadBalancer(ctx, identity)
	if err != nil {
		return cloud.Outcome{}, err
	}
	if managedLoadBalancer == nil {
		return cloud.ReadyOutcome(), nil
	}
	if !identity.MatchesGateway(managedLoadBalancer.Tags, roleLoadBalancer) {
		return cloud.Outcome{}, fmt.Errorf("%w: load balancer %s", cloud.ErrOwnershipConflict, managedLoadBalancer.ID)
	}
	observation, err := p.observeLoadBalancerOnce(ctx, managedLoadBalancer.ID)
	if err != nil {
		return cloud.Outcome{}, err
	}
	switch observation.phase {
	case loadBalancerPhaseAbsent:
		return cloud.ReadyOutcome(), nil
	case loadBalancerPhasePending:
		return observation.outcome, nil
	case loadBalancerPhaseActive:
		managedLoadBalancer = observation.loadBalancer
	default:
		return cloud.Outcome{}, fmt.Errorf("observe deleting Gateway load balancer: unknown internal phase %d", observation.phase)
	}
	deletionPlan, err := p.buildGatewayDeletionPlan(ctx, identity, managedLoadBalancer.ID)
	if err != nil {
		return cloud.Outcome{}, err
	}
	floatingIPPages, err := floatingips.List(p.clients.Network, floatingips.ListOpts{PortID: managedLoadBalancer.VipPortID}).AllPages(ctx)
	if err != nil {
		return cloud.Outcome{}, fmt.Errorf("list Floating IPs before Gateway deletion: %w", err)
	}
	floatingIPs, err := floatingips.ExtractFloatingIPs(floatingIPPages)
	if err != nil {
		return cloud.Outcome{}, fmt.Errorf("extract Floating IPs before Gateway deletion: %w", err)
	}
	for _, floatingIP := range floatingIPs {
		if err := p.validateFloatingIPProject(floatingIP.ID, floatingIP.ProjectID, floatingIP.TenantID); err != nil {
			return cloud.Outcome{}, err
		}
		if !identity.MatchesGatewayDescription(floatingIP.Description, roleFloatingIP) {
			return cloud.Outcome{}, fmt.Errorf("%w: Floating IP %s", cloud.ErrOwnershipConflict, floatingIP.ID)
		}
	}
	if len(deletionPlan) != 0 {
		step := deletionPlan[0]
		if err := p.executeGatewayDeletionStep(ctx, step); err != nil {
			return cloud.Outcome{}, err
		}
		return p.progressingOutcome(fmt.Sprintf("Deleting controller-owned %s", step.resource)), nil
	}
	if len(floatingIPs) != 0 {
		sort.Slice(floatingIPs, func(i, j int) bool { return floatingIPs[i].ID < floatingIPs[j].ID })
		if err := floatingips.Delete(ctx, p.clients.Network, floatingIPs[0].ID).ExtractErr(); err != nil && !isNotFound(err) {
			return cloud.Outcome{}, fmt.Errorf("delete Floating IP %s: %w", floatingIPs[0].ID, err)
		}
		return p.progressingOutcome("Deleting controller-owned Floating IP"), nil
	}
	if err := loadbalancers.Delete(ctx, p.clients.LoadBalancer, managedLoadBalancer.ID, nil).ExtractErr(); err != nil && !isNotFound(err) {
		return cloud.Outcome{}, fmt.Errorf("delete load balancer %s without cascade: %w", managedLoadBalancer.ID, classifyOctaviaMutationError(err))
	}
	return p.progressingOutcome("Deleting Octavia load balancer"), nil
}

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

type gatewayDeletionPlan []gatewayDeletionStep

func (p gatewayDeletionPlan) hasRouteResources() bool {
	for _, step := range p {
		switch step.resource {
		case gatewayDeletionL7Rule,
			gatewayDeletionL7Policy,
			gatewayDeletionMonitor,
			gatewayDeletionMember,
			gatewayDeletionPool:
			return true
		case gatewayDeletionListener:
		}
	}
	return false
}

// buildGatewayDeletionPlan validates the complete load balancer graph before
// returning any work. Steps are ordered so every child is deleted before its
// parent; callers can therefore execute the plan linearly and safely retry it.
func (p *Provider) buildGatewayDeletionPlan(ctx context.Context, identity Identity, loadBalancerID string) (gatewayDeletionPlan, error) {
	plan := gatewayDeletionPlan{}
	listenerSteps := gatewayDeletionPlan{}
	listenerPages, err := listeners.List(p.clients.LoadBalancer, listeners.ListOpts{LoadbalancerID: loadBalancerID}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list listeners before Gateway deletion: %w", err)
	}
	listenerList, err := listeners.ExtractListeners(listenerPages)
	if err != nil {
		return nil, fmt.Errorf("extract listeners before Gateway deletion: %w", err)
	}
	sort.Slice(listenerList, func(i, j int) bool { return listenerList[i].ID < listenerList[j].ID })
	for _, listener := range listenerList {
		if !identity.MatchesGateway(listener.Tags, roleListener) {
			return nil, fmt.Errorf("%w: listener %s", cloud.ErrOwnershipConflict, listener.ID)
		}
		policyPages, err := l7policies.List(p.clients.LoadBalancer, l7policies.ListOpts{ListenerID: listener.ID}).AllPages(ctx)
		if err != nil {
			return nil, fmt.Errorf("list L7 policies beneath listener %s before Gateway deletion: %w", listener.ID, err)
		}
		policyList, err := l7policies.ExtractL7Policies(policyPages)
		if err != nil {
			return nil, fmt.Errorf("extract L7 policies beneath listener %s before Gateway deletion: %w", listener.ID, err)
		}
		sort.Slice(policyList, func(i, j int) bool {
			if policyList[i].Position == policyList[j].Position {
				return policyList[i].ID < policyList[j].ID
			}
			return policyList[i].Position < policyList[j].Position
		})
		for _, policy := range policyList {
			if !matchesAnyGatewayRole(identity, policy.Tags, rolePolicyExact, rolePolicyPrefix) {
				return nil, fmt.Errorf("%w: L7 policy %s", cloud.ErrOwnershipConflict, policy.ID)
			}
			rulePages, err := l7policies.ListRules(p.clients.LoadBalancer, policy.ID, l7policies.ListRulesOpts{}).AllPages(ctx)
			if err != nil {
				return nil, fmt.Errorf("list L7 rules beneath policy %s before Gateway deletion: %w", policy.ID, err)
			}
			ruleList, err := l7policies.ExtractRules(rulePages)
			if err != nil {
				return nil, fmt.Errorf("extract L7 rules beneath policy %s before Gateway deletion: %w", policy.ID, err)
			}
			sort.Slice(ruleList, func(i, j int) bool { return ruleList[i].ID < ruleList[j].ID })
			for _, rule := range ruleList {
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
		listenerSteps = append(listenerSteps, gatewayDeletionStep{
			resource:   gatewayDeletionListener,
			resourceID: listener.ID,
		})
	}

	poolPages, err := pools.List(p.clients.LoadBalancer, pools.ListOpts{LoadbalancerID: loadBalancerID}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pools before Gateway deletion: %w", err)
	}
	poolList, err := pools.ExtractPools(poolPages)
	if err != nil {
		return nil, fmt.Errorf("extract pools before Gateway deletion: %w", err)
	}
	sort.Slice(poolList, func(i, j int) bool { return poolList[i].ID < poolList[j].ID })
	for _, pool := range poolList {
		if !identity.MatchesGateway(pool.Tags, rolePool) {
			return nil, fmt.Errorf("%w: pool %s", cloud.ErrOwnershipConflict, pool.ID)
		}
		monitorPages, err := monitors.List(p.clients.LoadBalancer, monitors.ListOpts{PoolID: pool.ID}).AllPages(ctx)
		if err != nil {
			return nil, fmt.Errorf("list health monitors beneath pool %s before Gateway deletion: %w", pool.ID, err)
		}
		monitorList, err := monitors.ExtractMonitors(monitorPages)
		if err != nil {
			return nil, fmt.Errorf("extract health monitors beneath pool %s before Gateway deletion: %w", pool.ID, err)
		}
		sort.Slice(monitorList, func(i, j int) bool { return monitorList[i].ID < monitorList[j].ID })
		for _, monitor := range monitorList {
			if !identity.MatchesGateway(monitor.Tags, roleMonitor) {
				return nil, fmt.Errorf("%w: monitor %s", cloud.ErrOwnershipConflict, monitor.ID)
			}
			plan = append(plan, gatewayDeletionStep{
				resource:   gatewayDeletionMonitor,
				resourceID: monitor.ID,
				parentID:   pool.ID,
			})
		}
		memberPages, err := pools.ListMembers(p.clients.LoadBalancer, pool.ID, pools.ListMembersOpts{}).AllPages(ctx)
		if err != nil {
			return nil, fmt.Errorf("list members beneath pool %s before Gateway deletion: %w", pool.ID, err)
		}
		memberList, err := pools.ExtractMembers(memberPages)
		if err != nil {
			return nil, fmt.Errorf("extract members beneath pool %s before Gateway deletion: %w", pool.ID, err)
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
		for _, member := range memberList {
			if !identity.MatchesGateway(member.Tags, roleMember) {
				return nil, fmt.Errorf("%w: member %s", cloud.ErrOwnershipConflict, member.ID)
			}
			plan = append(plan, gatewayDeletionStep{
				resource:   gatewayDeletionMember,
				resourceID: member.ID,
				parentID:   pool.ID,
			})
		}
		plan = append(plan, gatewayDeletionStep{
			resource:   gatewayDeletionPool,
			resourceID: pool.ID,
		})
	}
	plan = append(plan, listenerSteps...)
	return plan, nil
}

func (p *Provider) executeGatewayDeletionStep(ctx context.Context, step gatewayDeletionStep) error {
	deleteErr := p.deleteGatewayResource(ctx, step)
	if deleteErr != nil && !isNotFound(deleteErr) {
		return fmt.Errorf("delete %s %s: %w", step.resource, step.resourceID, classifyOctaviaMutationError(deleteErr))
	}
	return nil
}

func (p *Provider) deleteGatewayResource(ctx context.Context, step gatewayDeletionStep) error {
	switch step.resource {
	case gatewayDeletionL7Rule:
		return l7policies.DeleteRule(ctx, p.clients.LoadBalancer, step.parentID, step.resourceID).ExtractErr()
	case gatewayDeletionL7Policy:
		return l7policies.Delete(ctx, p.clients.LoadBalancer, step.resourceID).ExtractErr()
	case gatewayDeletionListener:
		return listeners.Delete(ctx, p.clients.LoadBalancer, step.resourceID).ExtractErr()
	case gatewayDeletionMonitor:
		return monitors.Delete(ctx, p.clients.LoadBalancer, step.resourceID).ExtractErr()
	case gatewayDeletionMember:
		return pools.DeleteMember(ctx, p.clients.LoadBalancer, step.parentID, step.resourceID).ExtractErr()
	case gatewayDeletionPool:
		return pools.Delete(ctx, p.clients.LoadBalancer, step.resourceID).ExtractErr()
	default:
		return fmt.Errorf("unsupported Gateway deletion resource %q", step.resource)
	}
}

func matchesAnyGatewayRole(identity Identity, tags []string, roles ...string) bool {
	for _, role := range roles {
		if identity.MatchesGateway(tags, role) {
			return true
		}
	}
	return false
}

func (p *Provider) findGatewayLoadBalancer(ctx context.Context, identity Identity) (*loadbalancers.LoadBalancer, error) {
	tags, err := identity.GatewayDiscoveryTags(roleLoadBalancer)
	if err != nil {
		return nil, err
	}
	pages, err := loadbalancers.List(p.clients.LoadBalancer, loadbalancers.ListOpts{ProjectID: p.clients.ProjectID, Tags: tags}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list managed load balancers: %w", err)
	}
	items, err := loadbalancers.ExtractLoadBalancers(pages)
	if err != nil {
		return nil, fmt.Errorf("extract managed load balancers: %w", err)
	}
	if len(items) > 1 {
		return nil, fmt.Errorf("%w: found %d load balancers for one Gateway", cloud.ErrOwnershipConflict, len(items))
	}
	if len(items) == 0 {
		return nil, nil
	}
	if err := p.validateLoadBalancerProject(items[0].ID, items[0].ProjectID); err != nil {
		return nil, err
	}
	return &items[0], nil
}
