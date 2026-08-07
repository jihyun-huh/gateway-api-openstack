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

package probe

import (
	"strings"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
	cloudopenstack "github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack"
)

const controllerIdentifier = cloudopenstack.Phase0ControllerIdentifier

// Identity preserves the Phase 0 state-file schema while delegating all tag
// and description behavior to the production OpenStack identity implementation.
type Identity struct {
	ClusterID        string `json:"clusterID"`
	Controller       string `json:"controller"`
	GatewayNamespace string `json:"gatewayNamespace"`
	GatewayName      string `json:"gatewayName"`
	GatewayUID       string `json:"gatewayUID"`
}

func NewIdentity(cfg Config) Identity {
	return Identity{
		ClusterID:        cfg.ClusterID,
		Controller:       controllerIdentifier,
		GatewayNamespace: cfg.GatewayNamespace,
		GatewayName:      cfg.GatewayName,
		GatewayUID:       cfg.GatewayUID,
	}
}

func (id Identity) cloudIdentity() cloud.Identity {
	return cloud.Identity{
		ClusterID:        id.ClusterID,
		Controller:       id.Controller,
		GatewayNamespace: id.GatewayNamespace,
		GatewayName:      id.GatewayName,
		GatewayUID:       id.GatewayUID,
	}
}

func (id Identity) Validate() error { return cloud.ValidateGatewayIdentity(id.cloudIdentity()) }

func (id Identity) Tags(role string) ([]string, error) {
	value, err := cloudopenstack.NewIdentity(id.cloudIdentity())
	if err != nil {
		return nil, err
	}
	return value.GatewayTags(role)
}

func (id Identity) Matches(tags []string, role string) bool {
	value, err := cloudopenstack.NewIdentity(id.cloudIdentity())
	return err == nil && value.MatchesGateway(tags, role)
}

func (id Identity) Description(role string) string {
	value, err := cloudopenstack.NewIdentity(id.cloudIdentity())
	if err != nil {
		return ""
	}
	return value.Phase0Description(role)
}

func (id Identity) MatchesDescription(description, role string) bool {
	value, err := cloudopenstack.NewIdentity(id.cloudIdentity())
	return err == nil && strings.HasPrefix(description, value.Phase0Description(role))
}
