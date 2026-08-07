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

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/l7policies"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/monitors"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

// DeleteRoute removes only resources carrying the complete Gateway and Route
// identity. It is safe to retry after a partial deletion.
func (p *Provider) DeleteRoute(ctx context.Context, value cloud.Identity) error {
	identity, err := p.identity(value)
	if err != nil {
		return err
	}
	loadBalancer, err := p.findGatewayLoadBalancer(ctx, identity)
	if err != nil || loadBalancer == nil {
		return err
	}
	if !identity.MatchesGateway(loadBalancer.Tags, roleLoadBalancer) {
		return fmt.Errorf("%w: load balancer %s", cloud.ErrOwnershipConflict, loadBalancer.ID)
	}
	loadBalancer, err = waitLoadBalancerActive(ctx, p.clients.LoadBalancer, loadBalancer.ID, p.operationTimeout, p.pollInterval)
	if err != nil {
		return err
	}

	poolTags, err := identity.RouteDiscoveryTags(rolePool)
	if err != nil {
		return err
	}
	pages, err := pools.List(p.clients.LoadBalancer, pools.ListOpts{LoadbalancerID: loadBalancer.ID, Tags: poolTags}).AllPages(ctx)
	if err != nil {
		return fmt.Errorf("list route pools for deletion: %w", err)
	}
	routePools, err := pools.ExtractPools(pages)
	if err != nil {
		return fmt.Errorf("extract route pools for deletion: %w", err)
	}
	if len(routePools) > 1 {
		return fmt.Errorf("%w: found duplicate pools during HTTPRoute deletion", cloud.ErrOwnershipConflict)
	}
	if len(routePools) == 1 && !identity.MatchesRoute(routePools[0].Tags, rolePool) {
		return fmt.Errorf("%w: pool %s", cloud.ErrOwnershipConflict, routePools[0].ID)
	}

	if err := p.deleteRoutePolicies(ctx, identity, loadBalancer.ID); err != nil {
		return err
	}
	if len(routePools) == 0 {
		return nil
	}
	pool := routePools[0]
	if err := p.deletePoolChildren(ctx, identity, loadBalancer.ID, pool.ID); err != nil {
		return err
	}
	if err := pools.Delete(ctx, p.clients.LoadBalancer, pool.ID).ExtractErr(); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete route pool %s: %w", pool.ID, err)
	}
	_, err = waitLoadBalancerActive(ctx, p.clients.LoadBalancer, loadBalancer.ID, p.operationTimeout, p.pollInterval)
	return err
}

func (p *Provider) deleteRoutePolicies(ctx context.Context, identity Identity, loadBalancerID string) error {
	listenerPages, err := listeners.List(p.clients.LoadBalancer, listeners.ListOpts{LoadbalancerID: loadBalancerID}).AllPages(ctx)
	if err != nil {
		return fmt.Errorf("list listeners before route deletion: %w", err)
	}
	listenerItems, err := listeners.ExtractListeners(listenerPages)
	if err != nil {
		return fmt.Errorf("extract listeners before route deletion: %w", err)
	}
	for _, listener := range listenerItems {
		if !identity.MatchesGateway(listener.Tags, roleListener) {
			return fmt.Errorf("%w: listener %s", cloud.ErrOwnershipConflict, listener.ID)
		}
		pages, err := l7policies.List(p.clients.LoadBalancer, l7policies.ListOpts{ListenerID: listener.ID}).AllPages(ctx)
		if err != nil {
			return fmt.Errorf("list route policies: %w", err)
		}
		policies, err := l7policies.ExtractL7Policies(pages)
		if err != nil {
			return fmt.Errorf("extract route policies: %w", err)
		}
		for _, policy := range policies {
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
				continue
			}
			rulePages, err := l7policies.ListRules(p.clients.LoadBalancer, policy.ID, l7policies.ListRulesOpts{}).AllPages(ctx)
			if err != nil {
				return fmt.Errorf("list policy rules before deletion: %w", err)
			}
			rules, err := l7policies.ExtractRules(rulePages)
			if err != nil {
				return fmt.Errorf("extract policy rules before deletion: %w", err)
			}
			for _, rule := range rules {
				if !identity.MatchesRoute(rule.Tags, roleRulePath) && !identity.MatchesRoute(rule.Tags, roleRuleHost) {
					return fmt.Errorf("%w: rule %s beneath managed policy %s", cloud.ErrOwnershipConflict, rule.ID, policy.ID)
				}
				if err := l7policies.DeleteRule(ctx, p.clients.LoadBalancer, policy.ID, rule.ID).ExtractErr(); err != nil && !isNotFound(err) {
					return fmt.Errorf("delete L7 rule %s: %w", rule.ID, err)
				}
				if _, err := waitLoadBalancerActive(ctx, p.clients.LoadBalancer, loadBalancerID, p.operationTimeout, p.pollInterval); err != nil {
					return err
				}
			}
			if err := l7policies.Delete(ctx, p.clients.LoadBalancer, policy.ID).ExtractErr(); err != nil && !isNotFound(err) {
				return fmt.Errorf("delete L7 policy %s: %w", policy.ID, err)
			}
			if _, err := waitLoadBalancerActive(ctx, p.clients.LoadBalancer, loadBalancerID, p.operationTimeout, p.pollInterval); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Provider) deletePoolChildren(ctx context.Context, identity Identity, loadBalancerID, poolID string) error {
	monitorPages, err := monitors.List(p.clients.LoadBalancer, monitors.ListOpts{PoolID: poolID}).AllPages(ctx)
	if err != nil {
		return fmt.Errorf("list monitors before deletion: %w", err)
	}
	monitorItems, err := monitors.ExtractMonitors(monitorPages)
	if err != nil {
		return fmt.Errorf("extract monitors before deletion: %w", err)
	}
	for _, monitor := range monitorItems {
		if !identity.MatchesRoute(monitor.Tags, roleMonitor) {
			return fmt.Errorf("%w: monitor %s", cloud.ErrOwnershipConflict, monitor.ID)
		}
		if err := monitors.Delete(ctx, p.clients.LoadBalancer, monitor.ID).ExtractErr(); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete monitor %s: %w", monitor.ID, err)
		}
		if _, err := waitLoadBalancerActive(ctx, p.clients.LoadBalancer, loadBalancerID, p.operationTimeout, p.pollInterval); err != nil {
			return err
		}
	}

	memberPages, err := pools.ListMembers(p.clients.LoadBalancer, poolID, pools.ListMembersOpts{}).AllPages(ctx)
	if err != nil {
		return fmt.Errorf("list members before deletion: %w", err)
	}
	members, err := pools.ExtractMembers(memberPages)
	if err != nil {
		return fmt.Errorf("extract members before deletion: %w", err)
	}
	for _, member := range members {
		if !identity.MatchesRoute(member.Tags, roleMember) {
			return fmt.Errorf("%w: member %s", cloud.ErrOwnershipConflict, member.ID)
		}
		if err := pools.DeleteMember(ctx, p.clients.LoadBalancer, poolID, member.ID).ExtractErr(); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete member %s: %w", member.ID, err)
		}
		if _, err := waitLoadBalancerActive(ctx, p.clients.LoadBalancer, loadBalancerID, p.operationTimeout, p.pollInterval); err != nil {
			return err
		}
	}
	return nil
}

// DeleteGateway removes the complete controller-owned graph. Descendants are
// validated and deleted explicitly before the load balancer is deleted without
// cascade, so a resource added after validation cannot be swept up implicitly.
func (p *Provider) DeleteGateway(ctx context.Context, cloudIdentity cloud.Identity) error {
	identity, err := p.identity(cloudIdentity)
	if err != nil {
		return err
	}
	managedLoadBalancer, err := p.findGatewayLoadBalancer(ctx, identity)
	if err != nil || managedLoadBalancer == nil {
		return err
	}
	if !identity.MatchesGateway(managedLoadBalancer.Tags, roleLoadBalancer) {
		return fmt.Errorf("%w: load balancer %s", cloud.ErrOwnershipConflict, managedLoadBalancer.ID)
	}
	managedLoadBalancer, err = waitLoadBalancerActive(ctx, p.clients.LoadBalancer, managedLoadBalancer.ID, p.operationTimeout, p.pollInterval)
	if err != nil {
		return err
	}
	deletionPlan, err := p.buildGatewayDeletionPlan(ctx, identity, managedLoadBalancer.ID)
	if err != nil {
		return err
	}
	floatingIPPages, err := floatingips.List(p.clients.Network, floatingips.ListOpts{PortID: managedLoadBalancer.VipPortID}).AllPages(ctx)
	if err != nil {
		return fmt.Errorf("list Floating IPs before Gateway deletion: %w", err)
	}
	floatingIPs, err := floatingips.ExtractFloatingIPs(floatingIPPages)
	if err != nil {
		return fmt.Errorf("extract Floating IPs before Gateway deletion: %w", err)
	}
	for _, floatingIP := range floatingIPs {
		if err := p.validateFloatingIPProject(floatingIP.ID, floatingIP.ProjectID, floatingIP.TenantID); err != nil {
			return err
		}
		if !identity.MatchesGatewayDescription(floatingIP.Description, roleFloatingIP) {
			return fmt.Errorf("%w: Floating IP %s", cloud.ErrOwnershipConflict, floatingIP.ID)
		}
	}
	if err := p.executeGatewayDeletionPlan(ctx, managedLoadBalancer.ID, deletionPlan); err != nil {
		return err
	}
	for _, floatingIP := range floatingIPs {
		if err := floatingips.Delete(ctx, p.clients.Network, floatingIP.ID).ExtractErr(); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete Floating IP %s: %w", floatingIP.ID, err)
		}
	}
	if err := loadbalancers.Delete(ctx, p.clients.LoadBalancer, managedLoadBalancer.ID, nil).ExtractErr(); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete load balancer %s without cascade: %w", managedLoadBalancer.ID, err)
	}
	return waitLoadBalancerDeleted(ctx, p.clients.LoadBalancer, managedLoadBalancer.ID, p.operationTimeout, p.pollInterval)
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

// buildGatewayDeletionPlan validates the complete load balancer graph before
// returning any work. Steps are ordered so every child is deleted before its
// parent; callers can therefore execute the plan linearly and safely retry it.
func (p *Provider) buildGatewayDeletionPlan(ctx context.Context, identity Identity, loadBalancerID string) (gatewayDeletionPlan, error) {
	plan := gatewayDeletionPlan{}
	listenerPages, err := listeners.List(p.clients.LoadBalancer, listeners.ListOpts{LoadbalancerID: loadBalancerID}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list listeners before Gateway deletion: %w", err)
	}
	listenerList, err := listeners.ExtractListeners(listenerPages)
	if err != nil {
		return nil, fmt.Errorf("extract listeners before Gateway deletion: %w", err)
	}
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
		plan = append(plan, gatewayDeletionStep{
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
	return plan, nil
}

// executeGatewayDeletionPlan has one execution loop. The plan builder owns the
// graph traversal and ordering, while this function owns delete/retry behavior.
func (p *Provider) executeGatewayDeletionPlan(ctx context.Context, loadBalancerID string, plan gatewayDeletionPlan) error {
	for _, step := range plan {
		deleteErr := p.deleteGatewayResource(ctx, step)
		if deleteErr != nil && !isNotFound(deleteErr) {
			return fmt.Errorf("delete %s %s: %w", step.resource, step.resourceID, deleteErr)
		}
		if _, err := waitLoadBalancerActive(ctx, p.clients.LoadBalancer, loadBalancerID, p.operationTimeout, p.pollInterval); err != nil {
			return fmt.Errorf("wait for load balancer %s after deleting %s %s: %w", loadBalancerID, step.resource, step.resourceID, err)
		}
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
