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
	"regexp"
	"sort"
	"strings"

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

// EnsureGateway creates or discovers the load balancer, optional Floating IP,
// and one HTTP listener owned by a Gateway.
func (p *Provider) EnsureGateway(ctx context.Context, spec cloud.GatewaySpec) (result cloud.GatewayResult, retErr error) {
	defer func() {
		retErr = classifyOpenStackError(retErr)
	}()

	ctx, cancel := p.operationContext(ctx)
	defer cancel()

	identity, err := p.identity(spec.Identity)
	if err != nil {
		return cloud.GatewayResult{}, err
	}
	if strings.TrimSpace(spec.VIPSubnetID) == "" {
		return cloud.GatewayResult{}, cloud.NewProviderError(cloud.ErrorCategoryTerminalValidation, errors.New("VIP subnet ID must not be empty"))
	}
	if spec.ListenerPort < 1 || spec.ListenerPort > 65535 {
		return cloud.GatewayResult{}, cloud.NewProviderError(
			cloud.ErrorCategoryTerminalValidation,
			fmt.Errorf("listener port %d is outside 1-65535", spec.ListenerPort),
		)
	}

	loadBalancer, outcome, err := p.ensureLoadBalancer(ctx, identity, spec)
	if err != nil {
		return cloud.GatewayResult{}, err
	}
	if outcome.State == cloud.OutcomeProgressing {
		return cloud.GatewayResult{Outcome: outcome}, nil
	}
	state := cloud.GatewayState{
		LoadBalancerID: loadBalancer.ID,
		VIPPortID:      loadBalancer.VipPortID,
		VIPAddress:     loadBalancer.VipAddress,
	}
	if _, err := p.buildGatewayDeletionPlan(ctx, identity, loadBalancer.ID); err != nil {
		return cloud.GatewayResult{}, fmt.Errorf("validate Gateway graph before mutation: %w", err)
	}
	observedListener, err := p.findGatewayListener(ctx, identity, spec, loadBalancer.ID)
	if err != nil {
		return cloud.GatewayResult{}, err
	}
	if spec.ExternalNetworkID != "" {
		floatingIP, floatingIPOutcome, ensureErr := p.ensureFloatingIP(ctx, identity, spec.ExternalNetworkID, loadBalancer.VipPortID)
		if ensureErr != nil {
			return cloud.GatewayResult{}, ensureErr
		}
		if floatingIPOutcome.State == cloud.OutcomeProgressing {
			return cloud.GatewayResult{State: state, Outcome: floatingIPOutcome}, nil
		}
		state.FloatingIPID = floatingIP.ID
		state.FloatingIPAddress = floatingIP.FloatingIP
	} else {
		floatingIPOutcome, err := p.ensureNoFloatingIP(ctx, identity, loadBalancer.VipPortID)
		if err != nil {
			return cloud.GatewayResult{}, err
		}
		if floatingIPOutcome.State == cloud.OutcomeProgressing {
			return cloud.GatewayResult{State: state, Outcome: floatingIPOutcome}, nil
		}
	}
	listener, listenerOutcome, err := p.ensureListener(ctx, identity, spec, loadBalancer.ID, observedListener)
	if err != nil {
		return cloud.GatewayResult{}, err
	}
	if listenerOutcome.State == cloud.OutcomeProgressing {
		return cloud.GatewayResult{State: state, Outcome: listenerOutcome}, nil
	}
	state.ListenerID = listener.ID
	return cloud.GatewayReadyResult(state), nil
}

func (p *Provider) ensureNoFloatingIP(ctx context.Context, identity Identity, vipPortID string) (cloud.Outcome, error) {
	pages, err := floatingips.List(p.clients.Network, floatingips.ListOpts{PortID: vipPortID}).AllPages(ctx)
	if err != nil {
		return cloud.Outcome{}, fmt.Errorf("list Floating IPs on VIP port: %w", err)
	}
	items, err := floatingips.ExtractFloatingIPs(pages)
	if err != nil {
		return cloud.Outcome{}, fmt.Errorf("extract Floating IPs: %w", err)
	}
	for _, item := range items {
		if err := p.validateFloatingIPProject(item.ID, item.ProjectID, item.TenantID); err != nil {
			return cloud.Outcome{}, err
		}
		if !identity.MatchesGatewayDescription(item.Description, roleFloatingIP) {
			return cloud.Outcome{}, fmt.Errorf("%w: VIP port %s has unmanaged Floating IP %s", cloud.ErrOwnershipConflict, vipPortID, item.ID)
		}
	}
	if len(items) == 0 {
		return cloud.ReadyOutcome(), nil
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	if err := floatingips.Delete(ctx, p.clients.Network, items[0].ID).ExtractErr(); err != nil && !isNotFound(err) {
		return cloud.Outcome{}, fmt.Errorf("delete no-longer-configured Floating IP %s: %w", items[0].ID, err)
	}
	return p.progressingOutcome("Removed a no-longer-configured Floating IP"), nil
}

// GetGateway discovers a complete, controller-owned Gateway without creating
// or mutating OpenStack resources. HTTPRoute reconciliation uses this method so
// it cannot race the Gateway reconciler and create a duplicate load balancer.
func (p *Provider) GetGateway(
	ctx context.Context,
	resourceIdentity cloud.Identity,
) (result cloud.GatewayResult, found bool, retErr error) {
	defer func() {
		retErr = classifyOpenStackError(retErr)
	}()

	ctx, cancel := p.operationContext(ctx)
	defer cancel()

	identity, err := p.identity(resourceIdentity)
	if err != nil {
		return cloud.GatewayResult{}, false, err
	}
	loadBalancer, err := p.findGatewayLoadBalancer(ctx, identity)
	if err != nil || loadBalancer == nil {
		return cloud.GatewayResult{}, false, err
	}
	if !identity.MatchesGateway(loadBalancer.Tags, roleLoadBalancer) {
		return cloud.GatewayResult{}, false, fmt.Errorf("%w: load balancer %s has an incomplete or stale identity", cloud.ErrOwnershipConflict, loadBalancer.ID)
	}
	observation, err := p.observeLoadBalancerOnce(ctx, loadBalancer.ID)
	if err != nil {
		return cloud.GatewayResult{}, false, err
	}
	switch observation.phase {
	case loadBalancerPhaseAbsent:
		return cloud.GatewayResult{}, false, nil
	case loadBalancerPhasePending:
		return cloud.GatewayResult{Outcome: observation.outcome}, true, nil
	case loadBalancerPhaseActive:
		loadBalancer = observation.loadBalancer
	default:
		return cloud.GatewayResult{}, false, fmt.Errorf("observe Gateway load balancer: unknown internal phase %d", observation.phase)
	}

	listener, err := p.findManagedGatewayListener(ctx, identity, loadBalancer.ID)
	if err != nil {
		return cloud.GatewayResult{}, false, err
	}
	if listener == nil {
		return cloud.GatewayProgressingResult("Octavia listener has not been created", p.pollInterval), true, nil
	}

	state := cloud.GatewayState{
		LoadBalancerID: loadBalancer.ID,
		VIPPortID:      loadBalancer.VipPortID,
		VIPAddress:     loadBalancer.VipAddress,
		ListenerID:     listener.ID,
	}
	floatingIPPages, err := floatingips.List(p.clients.Network, floatingips.ListOpts{PortID: loadBalancer.VipPortID}).AllPages(ctx)
	if err != nil {
		return cloud.GatewayResult{}, false, fmt.Errorf("list Gateway Floating IPs: %w", err)
	}
	floatingIPItems, err := floatingips.ExtractFloatingIPs(floatingIPPages)
	if err != nil {
		return cloud.GatewayResult{}, false, fmt.Errorf("extract Gateway Floating IPs: %w", err)
	}
	for _, item := range floatingIPItems {
		if err := p.validateFloatingIPProject(item.ID, item.ProjectID, item.TenantID); err != nil {
			return cloud.GatewayResult{}, false, err
		}
		if !identity.MatchesGatewayDescription(item.Description, roleFloatingIP) {
			return cloud.GatewayResult{}, false, fmt.Errorf("%w: VIP port %s has unmanaged Floating IP %s", cloud.ErrOwnershipConflict, loadBalancer.VipPortID, item.ID)
		}
		if state.FloatingIPID != "" {
			return cloud.GatewayResult{}, false, fmt.Errorf("%w: VIP port %s has multiple managed Floating IPs", cloud.ErrOwnershipConflict, loadBalancer.VipPortID)
		}
		state.FloatingIPID = item.ID
		state.FloatingIPAddress = item.FloatingIP
	}
	return cloud.GatewayReadyResult(state), true, nil
}

func (p *Provider) ensureLoadBalancer(
	ctx context.Context,
	identity Identity,
	spec cloud.GatewaySpec,
) (*loadbalancers.LoadBalancer, cloud.Outcome, error) {
	tags, err := identity.GatewayTags(roleLoadBalancer)
	if err != nil {
		return nil, cloud.Outcome{}, err
	}
	discoveryTags, err := identity.GatewayDiscoveryTags(roleLoadBalancer)
	if err != nil {
		return nil, cloud.Outcome{}, err
	}
	pages, err := loadbalancers.List(p.clients.LoadBalancer, loadbalancers.ListOpts{ProjectID: p.clients.ProjectID, Tags: discoveryTags}).AllPages(ctx)
	if err != nil {
		return nil, cloud.Outcome{}, fmt.Errorf("list managed load balancers: %w", err)
	}
	items, err := loadbalancers.ExtractLoadBalancers(pages)
	if err != nil {
		return nil, cloud.Outcome{}, fmt.Errorf("extract managed load balancers: %w", err)
	}
	if len(items) > 1 {
		return nil, cloud.Outcome{}, fmt.Errorf("%w: found %d load balancers for one Gateway", cloud.ErrOwnershipConflict, len(items))
	}
	if len(items) == 1 {
		item := items[0]
		if err := p.validateLoadBalancerProject(item.ID, item.ProjectID); err != nil {
			return nil, cloud.Outcome{}, err
		}
		if !identity.MatchesGateway(item.Tags, roleLoadBalancer) || item.VipSubnetID != spec.VIPSubnetID ||
			(spec.Provider != "" && item.Provider != spec.Provider) {
			return nil, cloud.Outcome{}, fmt.Errorf("%w: load balancer %s does not match the desired immutable fields", cloud.ErrOwnershipConflict, item.ID)
		}
		observation, err := p.observeLoadBalancerOnce(ctx, item.ID)
		if err != nil {
			return nil, cloud.Outcome{}, err
		}
		switch observation.phase {
		case loadBalancerPhaseAbsent:
			return nil, p.progressingOutcome("Octavia load balancer disappeared during observation"), nil
		case loadBalancerPhasePending:
			return nil, observation.outcome, nil
		case loadBalancerPhaseActive:
			observed := observation.loadBalancer
			if err := p.validateLoadBalancerProject(observed.ID, observed.ProjectID); err != nil {
				return nil, cloud.Outcome{}, err
			}
			if !identity.MatchesGateway(observed.Tags, roleLoadBalancer) || observed.VipSubnetID != spec.VIPSubnetID ||
				(spec.Provider != "" && observed.Provider != spec.Provider) {
				return nil, cloud.Outcome{}, fmt.Errorf("%w: load balancer %s changed immutable fields during observation", cloud.ErrOwnershipConflict, observed.ID)
			}
			return observed, cloud.ReadyOutcome(), nil
		default:
			return nil, cloud.Outcome{}, fmt.Errorf("observe managed load balancer: unknown internal phase %d", observation.phase)
		}
	}

	created, err := loadbalancers.Create(ctx, p.clients.LoadBalancer, loadbalancers.CreateOpts{
		Name:         resourceName(spec.Identity, roleLoadBalancer),
		Description:  identity.Description(roleLoadBalancer),
		VipSubnetID:  spec.VIPSubnetID,
		Provider:     spec.Provider,
		AdminStateUp: boolPointer(true),
		Tags:         tags,
	}).Extract()
	if err != nil {
		return nil, cloud.Outcome{}, fmt.Errorf("create load balancer: %w", classifyOctaviaMutationError(err))
	}
	if err := p.validateLoadBalancerProject(created.ID, created.ProjectID); err != nil {
		return nil, cloud.Outcome{}, err
	}
	return nil, p.progressingOutcome("Created Octavia load balancer"), nil
}

func (p *Provider) ensureListener(
	ctx context.Context,
	identity Identity,
	spec cloud.GatewaySpec,
	loadBalancerID string,
	observed *listeners.Listener,
) (*listeners.Listener, cloud.Outcome, error) {
	if observed != nil {
		return observed, cloud.ReadyOutcome(), nil
	}

	tags, err := identity.GatewayTags(roleListener)
	if err != nil {
		return nil, cloud.Outcome{}, err
	}
	createdListener, err := listeners.Create(ctx, p.clients.LoadBalancer, listeners.CreateOpts{
		LoadbalancerID: loadBalancerID,
		Protocol:       listeners.ProtocolHTTP,
		ProtocolPort:   spec.ListenerPort,
		Name:           spec.ListenerName,
		Description:    identity.Description(roleListener),
		AdminStateUp:   boolPointer(true),
		Tags:           tags,
	}).Extract()
	if err != nil {
		return nil, cloud.Outcome{}, fmt.Errorf("create listener: %w", classifyOctaviaMutationError(err))
	}
	return createdListener, p.progressingOutcome("Created Octavia listener"), nil
}

func (p *Provider) findGatewayListener(
	ctx context.Context,
	identity Identity,
	spec cloud.GatewaySpec,
	loadBalancerID string,
) (*listeners.Listener, error) {
	item, err := p.findManagedGatewayListener(ctx, identity, loadBalancerID)
	if err != nil || item == nil {
		return item, err
	}
	if item.Protocol != string(listeners.ProtocolHTTP) || item.ProtocolPort != spec.ListenerPort {
		return nil, fmt.Errorf("%w: listener %s has protocol %s/%d", cloud.ErrOwnershipConflict, item.ID, item.Protocol, item.ProtocolPort)
	}
	return item, nil
}

func (p *Provider) findManagedGatewayListener(
	ctx context.Context,
	identity Identity,
	loadBalancerID string,
) (*listeners.Listener, error) {
	pages, err := listeners.List(p.clients.LoadBalancer, listeners.ListOpts{
		LoadbalancerID: loadBalancerID,
		ProjectID:      p.clients.ProjectID,
	}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list Gateway listeners: %w", err)
	}
	listenerList, err := listeners.ExtractListeners(pages)
	if err != nil {
		return nil, fmt.Errorf("extract Gateway listeners: %w", err)
	}
	var owned *listeners.Listener
	for index := range listenerList {
		listener := &listenerList[index]
		if err := p.validateOptionalProject("listener", listener.ID, listener.ProjectID); err != nil {
			return nil, err
		}
		if !identity.MatchesGatewayDiscovery(listener.Tags, roleListener) || !identity.MatchesGateway(listener.Tags, roleListener) {
			return nil, fmt.Errorf("%w: load balancer %s contains listener %s with an incomplete or foreign identity", cloud.ErrOwnershipConflict, loadBalancerID, listener.ID)
		}
		if owned != nil {
			return nil, fmt.Errorf("%w: found multiple listeners for one Gateway", cloud.ErrOwnershipConflict)
		}
		for _, parent := range listener.Loadbalancers {
			if parent.ID != loadBalancerID {
				return nil, fmt.Errorf("%w: listener %s reports load balancer %s instead of %s", cloud.ErrOwnershipConflict, listener.ID, parent.ID, loadBalancerID)
			}
		}
		owned = listener
	}
	return owned, nil
}

func (p *Provider) ensureFloatingIP(
	ctx context.Context,
	identity Identity,
	networkID string,
	vipPortID string,
) (*floatingips.FloatingIP, cloud.Outcome, error) {
	pages, err := floatingips.List(p.clients.Network, floatingips.ListOpts{PortID: vipPortID}).AllPages(ctx)
	if err != nil {
		return nil, cloud.Outcome{}, fmt.Errorf("list Floating IPs on VIP port: %w", err)
	}
	floatingIPs, err := floatingips.ExtractFloatingIPs(pages)
	if err != nil {
		return nil, cloud.Outcome{}, fmt.Errorf("extract Floating IPs: %w", err)
	}
	var owned *floatingips.FloatingIP
	for index := range floatingIPs {
		fip := &floatingIPs[index]
		if err := p.validateFloatingIPProject(fip.ID, fip.ProjectID, fip.TenantID); err != nil {
			return nil, cloud.Outcome{}, err
		}
		if !identity.MatchesGatewayDescription(fip.Description, roleFloatingIP) {
			return nil, cloud.Outcome{}, fmt.Errorf("%w: VIP port %s already has unmanaged Floating IP %s", cloud.ErrOwnershipConflict, vipPortID, fip.ID)
		}
		if fip.FloatingNetworkID != networkID {
			return nil, cloud.Outcome{}, fmt.Errorf("%w: Floating IP %s on VIP port %s belongs to external network %s, not %s", cloud.ErrOwnershipConflict, fip.ID, vipPortID, fip.FloatingNetworkID, networkID)
		}
		if owned != nil {
			return nil, cloud.Outcome{}, fmt.Errorf("%w: VIP port %s has multiple managed Floating IPs", cloud.ErrOwnershipConflict, vipPortID)
		}
		owned = fip
	}
	if owned != nil {
		return owned, cloud.ReadyOutcome(), nil
	}
	created, err := floatingips.Create(ctx, p.clients.Network, floatingips.CreateOpts{
		Description:       identity.GatewayDescription(roleFloatingIP),
		FloatingNetworkID: networkID,
		PortID:            vipPortID,
	}).Extract()
	if err != nil {
		return nil, cloud.Outcome{}, fmt.Errorf("create Floating IP: %w", err)
	}
	if err := p.validateFloatingIPProject(created.ID, created.ProjectID, created.TenantID); err != nil {
		return nil, cloud.Outcome{}, err
	}
	return created, p.progressingOutcome("Allocated Floating IP"), nil
}

// Compile the expression once because resource names are normalized on every
// reconciliation.
var invalidOpenStackResourceNameCharacters = regexp.MustCompile(`[^A-Za-z0-9-]+`)

func resourceName(identity cloud.Identity, role string) string {
	name := fmt.Sprintf(
		"%s-%s-%s-%s",
		openStackResourceNamePrefix,
		identity.GatewayNamespace,
		identity.GatewayName,
		role,
	)
	name = invalidOpenStackResourceNameCharacters.ReplaceAllString(name, "-")

	if len(name) > 200 {
		return name[:200]
	}
	return name
}
