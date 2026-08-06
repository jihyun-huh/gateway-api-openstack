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
	"strings"

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

// EnsureGateway creates or discovers the load balancer, optional Floating IP,
// and one HTTP listener owned by a Gateway.
func (p *Provider) EnsureGateway(ctx context.Context, spec cloud.GatewaySpec) (cloud.GatewayState, error) {
	identity, err := p.identity(spec.Identity)
	if err != nil {
		return cloud.GatewayState{}, err
	}
	if strings.TrimSpace(spec.VIPSubnetID) == "" {
		return cloud.GatewayState{}, errors.New("VIP subnet ID must not be empty")
	}
	if spec.ListenerPort < 1 || spec.ListenerPort > 65535 {
		return cloud.GatewayState{}, fmt.Errorf("listener port %d is outside 1-65535", spec.ListenerPort)
	}

	loadBalancer, err := p.ensureLoadBalancer(ctx, identity, spec)
	if err != nil {
		return cloud.GatewayState{}, err
	}
	state := cloud.GatewayState{
		LoadBalancerID: loadBalancer.ID,
		VIPPortID:      loadBalancer.VipPortID,
		VIPAddress:     loadBalancer.VipAddress,
	}
	if spec.ExternalNetworkID != "" {
		floatingIP, ensureErr := p.ensureFloatingIP(ctx, identity, spec.ExternalNetworkID, loadBalancer.VipPortID)
		if ensureErr != nil {
			return cloud.GatewayState{}, ensureErr
		}
		state.FloatingIPID = floatingIP.ID
		state.FloatingIPAddress = floatingIP.FloatingIP
	} else if err := p.ensureNoFloatingIP(ctx, identity, loadBalancer.VipPortID); err != nil {
		return cloud.GatewayState{}, err
	}
	listener, err := p.ensureListener(ctx, identity, spec, loadBalancer.ID)
	if err != nil {
		return cloud.GatewayState{}, err
	}
	state.ListenerID = listener.ID
	return state, nil
}

func (p *Provider) ensureNoFloatingIP(ctx context.Context, identity Identity, vipPortID string) error {
	pages, err := floatingips.List(p.clients.Network, floatingips.ListOpts{PortID: vipPortID}).AllPages(ctx)
	if err != nil {
		return fmt.Errorf("list Floating IPs on VIP port: %w", err)
	}
	items, err := floatingips.ExtractFloatingIPs(pages)
	if err != nil {
		return fmt.Errorf("extract Floating IPs: %w", err)
	}
	for _, item := range items {
		if err := p.validateFloatingIPProject(item.ID, item.ProjectID, item.TenantID); err != nil {
			return err
		}
		if !identity.MatchesGatewayDescription(item.Description, roleFloatingIP) {
			return fmt.Errorf("%w: VIP port %s has unmanaged Floating IP %s", cloud.ErrOwnershipConflict, vipPortID, item.ID)
		}
	}
	for _, item := range items {
		if err := floatingips.Delete(ctx, p.clients.Network, item.ID).ExtractErr(); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete no-longer-configured Floating IP %s: %w", item.ID, err)
		}
	}
	return nil
}

// GetGateway discovers a complete, controller-owned Gateway without creating
// or mutating OpenStack resources. HTTPRoute reconciliation uses this method so
// it cannot race the Gateway reconciler and create a duplicate load balancer.
func (p *Provider) GetGateway(ctx context.Context, resourceIdentity cloud.Identity) (cloud.GatewayState, bool, error) {
	identity, err := p.identity(resourceIdentity)
	if err != nil {
		return cloud.GatewayState{}, false, err
	}
	loadBalancer, err := p.findGatewayLoadBalancer(ctx, identity)
	if err != nil || loadBalancer == nil {
		return cloud.GatewayState{}, false, err
	}
	if !identity.MatchesGateway(loadBalancer.Tags, roleLoadBalancer) {
		return cloud.GatewayState{}, false, fmt.Errorf("%w: load balancer %s has an incomplete or stale identity", cloud.ErrOwnershipConflict, loadBalancer.ID)
	}
	loadBalancer, err = waitLoadBalancerActive(ctx, p.clients.LoadBalancer, loadBalancer.ID, p.operationTimeout, p.pollInterval)
	if err != nil {
		return cloud.GatewayState{}, false, err
	}

	listenerPages, err := listeners.List(p.clients.LoadBalancer, listeners.ListOpts{LoadbalancerID: loadBalancer.ID}).AllPages(ctx)
	if err != nil {
		return cloud.GatewayState{}, false, fmt.Errorf("list Gateway listeners: %w", err)
	}
	listenerItems, err := listeners.ExtractListeners(listenerPages)
	if err != nil {
		return cloud.GatewayState{}, false, fmt.Errorf("extract Gateway listeners: %w", err)
	}
	var listener *listeners.Listener
	for index := range listenerItems {
		item := &listenerItems[index]
		if !identity.MatchesGateway(item.Tags, roleListener) {
			return cloud.GatewayState{}, false, fmt.Errorf("%w: load balancer %s contains unmanaged listener %s", cloud.ErrOwnershipConflict, loadBalancer.ID, item.ID)
		}
		if listener != nil {
			return cloud.GatewayState{}, false, fmt.Errorf("%w: load balancer %s has multiple managed listeners", cloud.ErrOwnershipConflict, loadBalancer.ID)
		}
		listener = item
	}
	if listener == nil {
		return cloud.GatewayState{}, false, nil
	}

	state := cloud.GatewayState{
		LoadBalancerID: loadBalancer.ID,
		VIPPortID:      loadBalancer.VipPortID,
		VIPAddress:     loadBalancer.VipAddress,
		ListenerID:     listener.ID,
	}
	floatingIPPages, err := floatingips.List(p.clients.Network, floatingips.ListOpts{PortID: loadBalancer.VipPortID}).AllPages(ctx)
	if err != nil {
		return cloud.GatewayState{}, false, fmt.Errorf("list Gateway Floating IPs: %w", err)
	}
	floatingIPItems, err := floatingips.ExtractFloatingIPs(floatingIPPages)
	if err != nil {
		return cloud.GatewayState{}, false, fmt.Errorf("extract Gateway Floating IPs: %w", err)
	}
	for _, item := range floatingIPItems {
		if err := p.validateFloatingIPProject(item.ID, item.ProjectID, item.TenantID); err != nil {
			return cloud.GatewayState{}, false, err
		}
		if !identity.MatchesGatewayDescription(item.Description, roleFloatingIP) {
			return cloud.GatewayState{}, false, fmt.Errorf("%w: VIP port %s has unmanaged Floating IP %s", cloud.ErrOwnershipConflict, loadBalancer.VipPortID, item.ID)
		}
		if state.FloatingIPID != "" {
			return cloud.GatewayState{}, false, fmt.Errorf("%w: VIP port %s has multiple managed Floating IPs", cloud.ErrOwnershipConflict, loadBalancer.VipPortID)
		}
		state.FloatingIPID = item.ID
		state.FloatingIPAddress = item.FloatingIP
	}
	return state, true, nil
}

func (p *Provider) ensureLoadBalancer(ctx context.Context, identity Identity, spec cloud.GatewaySpec) (*loadbalancers.LoadBalancer, error) {
	tags, err := identity.GatewayTags(roleLoadBalancer)
	if err != nil {
		return nil, err
	}
	discoveryTags, err := identity.GatewayDiscoveryTags(roleLoadBalancer)
	if err != nil {
		return nil, err
	}
	pages, err := loadbalancers.List(p.clients.LoadBalancer, loadbalancers.ListOpts{ProjectID: p.clients.ProjectID, Tags: discoveryTags}).AllPages(ctx)
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
	if len(items) == 1 {
		item := items[0]
		if err := p.validateLoadBalancerProject(item.ID, item.ProjectID); err != nil {
			return nil, err
		}
		if !identity.MatchesGateway(item.Tags, roleLoadBalancer) || item.VipSubnetID != spec.VIPSubnetID ||
			(spec.Provider != "" && item.Provider != spec.Provider) {
			return nil, fmt.Errorf("%w: load balancer %s does not match the desired immutable fields", cloud.ErrOwnershipConflict, item.ID)
		}
		return waitLoadBalancerActive(ctx, p.clients.LoadBalancer, item.ID, p.operationTimeout, p.pollInterval)
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
		return nil, fmt.Errorf("create load balancer: %w", err)
	}
	if err := p.validateLoadBalancerProject(created.ID, created.ProjectID); err != nil {
		return nil, err
	}
	return waitLoadBalancerActive(ctx, p.clients.LoadBalancer, created.ID, p.operationTimeout, p.pollInterval)
}

func (p *Provider) ensureListener(ctx context.Context, identity Identity, spec cloud.GatewaySpec, loadBalancerID string) (*listeners.Listener, error) {
	tags, err := identity.GatewayTags(roleListener)
	if err != nil {
		return nil, err
	}
	pages, err := listeners.List(p.clients.LoadBalancer, listeners.ListOpts{LoadbalancerID: loadBalancerID}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list listeners: %w", err)
	}
	listenerList, err := listeners.ExtractListeners(pages)
	if err != nil {
		return nil, fmt.Errorf("extract listeners: %w", err)
	}
	var owned []listeners.Listener
	for _, listener := range listenerList {
		if !identity.MatchesGatewayDiscovery(listener.Tags, roleListener) {
			return nil, fmt.Errorf("%w: load balancer %s contains unmanaged listener %s", cloud.ErrOwnershipConflict, loadBalancerID, listener.ID)
		}
		if !identity.MatchesGateway(listener.Tags, roleListener) {
			return nil, fmt.Errorf("%w: listener %s has an incomplete or stale identity", cloud.ErrOwnershipConflict, listener.ID)
		}
		owned = append(owned, listener)
	}
	if len(owned) > 1 {
		return nil, fmt.Errorf("%w: found %d listeners for one Gateway", cloud.ErrOwnershipConflict, len(owned))
	}
	if len(owned) == 1 {
		item := owned[0]
		if item.Protocol != string(listeners.ProtocolHTTP) || item.ProtocolPort != spec.ListenerPort {
			return nil, fmt.Errorf("%w: listener %s has protocol %s/%d", cloud.ErrOwnershipConflict, item.ID, item.Protocol, item.ProtocolPort)
		}
		return &item, nil
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
		return nil, fmt.Errorf("create listener: %w", err)
	}
	if _, err := waitLoadBalancerActive(ctx, p.clients.LoadBalancer, loadBalancerID, p.operationTimeout, p.pollInterval); err != nil {
		return nil, err
	}
	return createdListener, nil
}

func (p *Provider) ensureFloatingIP(ctx context.Context, identity Identity, networkID, vipPortID string) (*floatingips.FloatingIP, error) {
	pages, err := floatingips.List(p.clients.Network, floatingips.ListOpts{PortID: vipPortID}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list Floating IPs on VIP port: %w", err)
	}
	floatingIPs, err := floatingips.ExtractFloatingIPs(pages)
	if err != nil {
		return nil, fmt.Errorf("extract Floating IPs: %w", err)
	}
	var owned *floatingips.FloatingIP
	for index := range floatingIPs {
		fip := &floatingIPs[index]
		if err := p.validateFloatingIPProject(fip.ID, fip.ProjectID, fip.TenantID); err != nil {
			return nil, err
		}
		if !identity.MatchesGatewayDescription(fip.Description, roleFloatingIP) {
			return nil, fmt.Errorf("%w: VIP port %s already has unmanaged Floating IP %s", cloud.ErrOwnershipConflict, vipPortID, fip.ID)
		}
		if fip.FloatingNetworkID != networkID {
			return nil, fmt.Errorf("%w: Floating IP %s on VIP port %s belongs to external network %s, not %s", cloud.ErrOwnershipConflict, fip.ID, vipPortID, fip.FloatingNetworkID, networkID)
		}
		if owned != nil {
			return nil, fmt.Errorf("%w: VIP port %s has multiple managed Floating IPs", cloud.ErrOwnershipConflict, vipPortID)
		}
		owned = fip
	}
	if owned != nil {
		return owned, nil
	}
	created, err := floatingips.Create(ctx, p.clients.Network, floatingips.CreateOpts{
		Description:       identity.GatewayDescription(roleFloatingIP),
		FloatingNetworkID: networkID,
		PortID:            vipPortID,
	}).Extract()
	if err != nil {
		return nil, fmt.Errorf("create Floating IP: %w", err)
	}
	if err := p.validateFloatingIPProject(created.ID, created.ProjectID, created.TenantID); err != nil {
		return nil, err
	}
	return created, nil
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
