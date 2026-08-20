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

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
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
	if observation.loadBalancer != nil {
		if observation.loadBalancer.ID != managedLoadBalancer.ID {
			return cloud.Outcome{}, fmt.Errorf(
				"%w: requested load balancer %s but OpenStack returned %s",
				cloud.ErrOwnershipConflict,
				managedLoadBalancer.ID,
				observation.loadBalancer.ID,
			)
		}
		if err := p.validateLoadBalancerProject(observation.loadBalancer.ID, observation.loadBalancer.ProjectID); err != nil {
			return cloud.Outcome{}, err
		}
		if !identity.MatchesGateway(observation.loadBalancer.Tags, roleLoadBalancer) {
			return cloud.Outcome{}, fmt.Errorf("%w: load balancer %s changed identity after discovery", cloud.ErrOwnershipConflict, observation.loadBalancer.ID)
		}
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
	floatingIPs, err := p.neutron.ListFloatingIPs(ctx, floatingips.ListOpts{
		PortID:    managedLoadBalancer.VipPortID,
		ProjectID: p.projectID,
	})
	if err != nil {
		return cloud.Outcome{}, fmt.Errorf("list Floating IPs before Gateway deletion: %w", err)
	}
	for _, floatingIP := range floatingIPs {
		if err := p.validateFloatingIPProject(floatingIP.ID, floatingIP.ProjectID, floatingIP.TenantID); err != nil {
			return cloud.Outcome{}, err
		}
		if !identity.MatchesGatewayDescription(floatingIP.Description, roleFloatingIP) {
			return cloud.Outcome{}, fmt.Errorf("%w: Floating IP %s", cloud.ErrOwnershipConflict, floatingIP.ID)
		}
	}
	if step, ok := deletionPlan.firstStep(); ok {
		if err := p.executeGatewayDeletionStep(ctx, step); err != nil {
			return cloud.Outcome{}, err
		}
		return p.progressingOutcome(fmt.Sprintf("Deleting controller-owned %s", step.resource)), nil
	}
	if len(floatingIPs) != 0 {
		sort.Slice(floatingIPs, func(i, j int) bool { return floatingIPs[i].ID < floatingIPs[j].ID })
		if err := p.neutron.DeleteFloatingIP(ctx, floatingIPs[0].ID); err != nil && !isNotFound(err) {
			return cloud.Outcome{}, fmt.Errorf("delete Floating IP %s: %w", floatingIPs[0].ID, err)
		}
		return p.progressingOutcome("Deleting controller-owned Floating IP"), nil
	}
	if err := p.octavia.DeleteLoadBalancer(ctx, managedLoadBalancer.ID); err != nil && !isNotFound(err) {
		return cloud.Outcome{}, fmt.Errorf("delete load balancer %s without cascade: %w", managedLoadBalancer.ID, classifyOctaviaMutationError(err))
	}
	return p.progressingOutcome("Deleting Octavia load balancer"), nil
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
		return p.octavia.DeleteRule(ctx, step.parentID, step.resourceID)
	case gatewayDeletionL7Policy:
		return p.octavia.DeletePolicy(ctx, step.resourceID)
	case gatewayDeletionListener:
		return p.octavia.DeleteListener(ctx, step.resourceID)
	case gatewayDeletionMonitor:
		return p.octavia.DeleteMonitor(ctx, step.resourceID)
	case gatewayDeletionMember:
		return p.octavia.DeleteMember(ctx, step.parentID, step.resourceID)
	case gatewayDeletionPool:
		return p.octavia.DeletePool(ctx, step.resourceID)
	default:
		return fmt.Errorf("unsupported Gateway deletion resource %q", step.resource)
	}
}

func (p *Provider) findGatewayLoadBalancer(ctx context.Context, identity Identity) (*loadbalancers.LoadBalancer, error) {
	tags, err := identity.GatewayDiscoveryTags(roleLoadBalancer)
	if err != nil {
		return nil, err
	}
	items, err := p.octavia.ListLoadBalancers(ctx, loadbalancers.ListOpts{ProjectID: p.projectID, Tags: tags})
	if err != nil {
		return nil, fmt.Errorf("list managed load balancers: %w", err)
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
