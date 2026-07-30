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
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const (
	identityPrefix       = "gako."
	controllerIdentifier = "openstack-gateway-controller"
	maxTagLength         = 255
)

// Identity is the immutable ownership tuple carried by every probe resource.
// It mirrors the minimum identity that the future controller must enforce.
type Identity struct {
	ClusterID        string `json:"clusterID"`
	Controller       string `json:"controller"`
	GatewayNamespace string `json:"gatewayNamespace"`
	GatewayName      string `json:"gatewayName"`
	GatewayUID       string `json:"gatewayUID"`
}

// NewIdentity builds an identity from a validated probe configuration.
func NewIdentity(cfg Config) Identity {
	return Identity{
		ClusterID:        cfg.ClusterID,
		Controller:       controllerIdentifier,
		GatewayNamespace: cfg.GatewayNamespace,
		GatewayName:      cfg.GatewayName,
		GatewayUID:       cfg.GatewayUID,
	}
}

// Validate rejects an identity that cannot safely distinguish ownership.
func (id Identity) Validate() error {
	for name, value := range map[string]string{
		"cluster ID":        id.ClusterID,
		"controller":        id.Controller,
		"gateway namespace": id.GatewayNamespace,
		"gateway name":      id.GatewayName,
		"gateway UID":       id.GatewayUID,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s must not be empty", name)
		}
	}
	return nil
}

// Tags encodes the full identity and the resource role as Octavia tags.
func (id Identity) Tags(role string) ([]string, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(role) == "" {
		return nil, errors.New("resource role must not be empty")
	}

	tags := []string{
		identityPrefix + "managed=true",
		tag("cluster", id.ClusterID),
		tag("controller", id.Controller),
		tag("namespace", id.GatewayNamespace),
		tag("name", id.GatewayName),
		tag("uid", id.GatewayUID),
		tag("role", role),
	}
	for _, value := range tags {
		if len(value) > maxTagLength {
			return nil, fmt.Errorf("encoded identity tag exceeds %d bytes", maxTagLength)
		}
	}
	return tags, nil
}

// Matches returns true only when every expected identity tag is present.
func (id Identity) Matches(tags []string, role string) bool {
	expected, err := id.Tags(role)
	if err != nil {
		return false
	}
	actual := make(map[string]struct{}, len(tags))
	for _, value := range tags {
		actual[value] = struct{}{}
	}
	for _, value := range expected {
		if _, ok := actual[value]; !ok {
			return false
		}
	}
	return true
}

// Description returns the stable prefix used to identify a Neutron Floating IP.
// The digest covers the complete identity and role while keeping the value
// within Neutron description limits.
func (id Identity) Description(role string) string {
	var digestInput strings.Builder
	for _, value := range []string{
		id.ClusterID,
		id.Controller,
		id.GatewayNamespace,
		id.GatewayName,
		id.GatewayUID,
		role,
	} {
		fmt.Fprintf(&digestInput, "%d:", len(value))
		digestInput.WriteString(value)
	}
	digest := sha256.Sum256([]byte(digestInput.String()))

	return fmt.Sprintf(
		"gateway-api-openstack phase-0; identity-sha256=%x; role=%s",
		digest,
		encode(role),
	)
}

// MatchesDescription validates the immutable portion of a Floating IP description.
func (id Identity) MatchesDescription(description, role string) bool {
	return strings.HasPrefix(description, id.Description(role))
}

func tag(key, value string) string {
	return identityPrefix + key + "=" + encode(value)
}

func encode(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}
