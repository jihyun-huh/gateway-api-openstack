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

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack/clients"
	openstackidentity "github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack/identity"
)

const (
	// DefaultAPIQPS is the steady process-wide OpenStack request rate.
	DefaultAPIQPS = clients.DefaultAPIQPS
	// DefaultAPIBurst allows short request bursts within the process-wide rate
	// limit.
	DefaultAPIBurst = clients.DefaultAPIBurst
	// Phase0ControllerIdentifier preserves the capability probe identity.
	Phase0ControllerIdentifier = openstackidentity.Phase0ControllerIdentifier
)

// ClientConfig is retained at the adapter facade for command compatibility.
type ClientConfig = clients.ClientConfig

// ServiceClients is retained at the adapter facade for command and probe
// compatibility.
type ServiceClients = clients.ServiceClients

// Identity is retained at the adapter facade for the Phase 0 probe.
type Identity = openstackidentity.Identity

// NewServiceClients authenticates and creates the shared OpenStack clients.
func NewServiceClients(ctx context.Context, cfg ClientConfig) (ServiceClients, error) {
	return clients.NewServiceClients(ctx, cfg)
}

// ValidateMicroversion validates the Octavia microversion required for
// ownership tags.
func ValidateMicroversion(value string) error {
	return clients.ValidateMicroversion(value)
}

// NewIdentity wraps one provider-neutral immutable identity.
func NewIdentity(value cloud.Identity) (Identity, error) {
	return openstackidentity.NewIdentity(value)
}
