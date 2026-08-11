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

// Package openstack implements the cloud.Provider boundary with Octavia and
// Neutron. Kubernetes types intentionally do not enter this package.
package openstack

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

const (
	roleLoadBalancer = "loadbalancer"
	roleListener     = "listener"
	roleFloatingIP   = "floating-ip"
	rolePool         = "pool"
	roleMember       = "member"
	roleMonitor      = "health-monitor"
	rolePolicyExact  = "l7-policy-exact"
	rolePolicyPrefix = "l7-policy-prefix"
	roleRulePath     = "l7-rule-path"
	roleRuleHost     = "l7-rule-host"
)

// ProviderConfig bounds one provider call and controls how soon the controller
// observes an asynchronous Octavia operation again.
type ProviderConfig struct {
	OperationTimeout time.Duration
	PollInterval     time.Duration
}

// Provider owns the OpenStack resource graph for managed Gateway API objects.
type Provider struct {
	clients          ServiceClients
	operationTimeout time.Duration
	pollInterval     time.Duration
}

// NewProvider creates an OpenStack provider. The clients are expected to have
// an Octavia microversion that supports tags (2.5 or later).
func NewProvider(clients ServiceClients, cfg ProviderConfig) *Provider {
	if cfg.OperationTimeout <= 0 {
		cfg.OperationTimeout = 10 * time.Minute
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	return &Provider{
		clients:          clients,
		operationTimeout: cfg.OperationTimeout,
		pollInterval:     cfg.PollInterval,
	}
}

var _ cloud.Provider = (*Provider)(nil)

func (p *Provider) identity(resourceIdentity cloud.Identity) (Identity, error) {
	if strings.TrimSpace(p.clients.ProjectID) == "" {
		return Identity{}, fmt.Errorf("authenticated OpenStack project ID is missing from service clients")
	}
	if strings.TrimSpace(resourceIdentity.OpenStackProjectID) == "" {
		return Identity{}, fmt.Errorf("controller resource identity must include an OpenStack project ID")
	}
	if resourceIdentity.OpenStackProjectID != p.clients.ProjectID {
		return Identity{}, fmt.Errorf("%w: identity project %s does not match authenticated project %s", cloud.ErrOwnershipConflict, resourceIdentity.OpenStackProjectID, p.clients.ProjectID)
	}
	return NewIdentity(resourceIdentity)
}

func (p *Provider) validateLoadBalancerProject(id, projectID string) error {
	if projectID != p.clients.ProjectID {
		return fmt.Errorf("%w: load balancer %s belongs to project %s, not authenticated project %s", cloud.ErrOwnershipConflict, id, projectID, p.clients.ProjectID)
	}
	return nil
}

func (p *Provider) validateFloatingIPProject(id, projectID, tenantID string) error {
	if projectID == "" {
		projectID = tenantID
	}
	if projectID != p.clients.ProjectID {
		return fmt.Errorf("%w: Floating IP %s belongs to project %s, not authenticated project %s", cloud.ErrOwnershipConflict, id, projectID, p.clients.ProjectID)
	}
	return nil
}

func (p *Provider) validateOptionalProject(resource, id, projectID string) error {
	if projectID != "" && projectID != p.clients.ProjectID {
		return fmt.Errorf("%w: %s %s belongs to project %s, not authenticated project %s", cloud.ErrOwnershipConflict, resource, id, projectID, p.clients.ProjectID)
	}
	return nil
}

func (p *Provider) operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, p.operationTimeout)
}

func boolPointer(value bool) *bool { return &value }
