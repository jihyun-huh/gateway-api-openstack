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
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

const (
	identityPrefix = "gateway-api-openstack."
	maxTagLength   = 255

	// Phase0ControllerIdentifier preserves the identity used by capability
	// probe resources. It is deliberately not a Gateway API controller name.
	Phase0ControllerIdentifier = "openstack-gateway-controller"

	openStackResourceNamePrefix = "gateway-api-openstack"
)

// Identity encodes the shared immutable ownership tuple into Octavia tags and
// Neutron descriptions.
type Identity struct {
	value cloud.Identity
}

// NewIdentity validates and wraps a provider-neutral identity.
func NewIdentity(resourceIdentity cloud.Identity) (Identity, error) {
	if err := cloud.ValidateGatewayIdentity(resourceIdentity); err != nil {
		return Identity{}, err
	}
	return Identity{value: resourceIdentity}, nil
}

// GatewayTags returns the complete Gateway ownership tuple for a resource.
func (id Identity) GatewayTags(role string) ([]string, error) {
	return id.tags(role, false, true)
}

// RouteTags returns the Gateway and HTTPRoute ownership tuples for a
// route-scoped resource.
func (id Identity) RouteTags(role string) ([]string, error) {
	if err := cloud.ValidateRouteIdentity(id.value); err != nil {
		return nil, err
	}
	return id.tags(role, true, true)
}

// GatewayDiscoveryTags return the stable, name-scoped portion of Gateway
// identity. Keep this query deliberately narrower than the exact identity so
// adding a new immutable field in a controller upgrade discovers an older
// candidate and reports a conflict instead of creating a duplicate. A
// candidate found with these tags must still pass MatchesGateway.
func (id Identity) GatewayDiscoveryTags(role string) ([]string, error) {
	if strings.TrimSpace(role) == "" {
		return nil, fmt.Errorf("resource role must not be empty")
	}
	tags := []string{
		identityPrefix + "managed=true",
		tag("cluster", id.value.ClusterID),
		tag("controller", id.value.Controller),
		tag("namespace", id.value.GatewayNamespace),
		tag("name", id.value.GatewayName),
		tag("role", role),
	}
	return validateTagLengths(tags)
}

// RouteDiscoveryTags return stable Gateway and Route names without their UIDs.
// Exact UID matching remains mandatory before reuse, mutation, or deletion.
func (id Identity) RouteDiscoveryTags(role string) ([]string, error) {
	if err := cloud.ValidateRouteIdentity(id.value); err != nil {
		return nil, err
	}
	tags, err := id.GatewayDiscoveryTags(role)
	if err != nil {
		return nil, err
	}
	return validateTagLengths(append(tags,
		tag("route-namespace", id.value.RouteNamespace),
		tag("route-name", id.value.RouteName),
	))
}

// MatchesGateway requires the complete Gateway tuple and role. Additional
// route tags are allowed so Gateway finalization can remove descendants.
func (id Identity) MatchesGateway(tags []string, role string) bool {
	expected, err := id.tags(role, false, false)
	return err == nil && containsAll(tags, expected)
}

// MatchesGatewayDiscovery reports whether tags identify a candidate with the
// same stable Gateway name tuple. It is never sufficient authorization for a
// mutation; callers must then require MatchesGateway.
func (id Identity) MatchesGatewayDiscovery(tags []string, role string) bool {
	expected, err := id.GatewayDiscoveryTags(role)
	return err == nil && containsAll(tags, expected)
}

// MatchesRoute requires both the Gateway and HTTPRoute tuples and role.
func (id Identity) MatchesRoute(tags []string, role string) bool {
	if err := cloud.ValidateRouteIdentity(id.value); err != nil {
		return false
	}
	expected, err := id.tags(role, true, false)
	return err == nil && containsAll(tags, expected)
}

// MatchesRouteDiscovery reports whether tags identify a route candidate while
// deliberately ignoring immutable UIDs. Exact matching remains mandatory
// before reuse, mutation, or deletion.
func (id Identity) MatchesRouteDiscovery(tags []string, role string) bool {
	expected, err := id.RouteDiscoveryTags(role)
	return err == nil && containsAll(tags, expected)
}

// Description returns a bounded digest for Neutron resources that do not
// expose tags through the API used by this project.
func (id Identity) Description(role string) string {
	description := id.description("", role, true, true)
	if id.value.ControllerVersion != "" {
		version := encode(id.value.ControllerVersion)
		if len(description)+len("; controller-version=")+len(version) > maxTagLength {
			digest := sha256.Sum256([]byte(id.value.ControllerVersion))
			version = "sha256-" + base64.RawURLEncoding.EncodeToString(digest[:])
		}
		description += "; controller-version=" + version
	}
	return description
}

// GatewayDescription removes route fields before computing the description.
// This is required for Gateway-scoped Neutron resources when a caller happens
// to hold the richer HTTPRoute identity for the same Gateway.
func (id Identity) GatewayDescription(role string) string {
	gatewayIdentity := id
	gatewayIdentity.value.RouteNamespace = ""
	gatewayIdentity.value.RouteName = ""
	gatewayIdentity.value.RouteUID = ""
	return gatewayIdentity.Description(role)
}

// Phase0Description preserves the description format and digest used by the
// original capability probe so an interrupted older run can still be cleaned
// up after upgrading the source tree.
func (id Identity) Phase0Description(role string) string {
	return id.description("phase-0", role, false, false)
}

func (id Identity) description(scope, role string, includeRoute, includeProject bool) string {
	parts := []string{
		id.value.ClusterID, id.value.Controller,
	}
	if includeProject {
		parts = append(parts, id.value.OpenStackProjectID)
	}
	parts = append(parts, id.value.GatewayNamespace, id.value.GatewayName, id.value.GatewayUID)
	if includeRoute {
		parts = append(parts, id.value.RouteNamespace, id.value.RouteName, id.value.RouteUID)
	}
	parts = append(parts, role)

	var input strings.Builder
	for _, value := range parts {
		fmt.Fprintf(&input, "%d:", len(value))
		input.WriteString(value)
	}
	digest := sha256.Sum256([]byte(input.String()))
	prefix := "gateway-api-openstack"
	if scope != "" {
		prefix += " " + scope
	}
	return fmt.Sprintf("%s; identity-sha256=%x; role=%s", prefix, digest, encode(role))
}

// MatchesDescription validates the immutable portion of a Neutron resource
// description while allowing a human-readable suffix.
func (id Identity) MatchesDescription(description, role string) bool {
	return strings.HasPrefix(description, id.description("", role, true, true))
}

// MatchesGatewayDescription validates a Gateway-scoped Neutron description
// independently of any HTTPRoute fields present in this Identity value.
func (id Identity) MatchesGatewayDescription(description, role string) bool {
	gatewayIdentity := id
	gatewayIdentity.value.RouteNamespace = ""
	gatewayIdentity.value.RouteName = ""
	gatewayIdentity.value.RouteUID = ""
	return gatewayIdentity.MatchesDescription(description, role)
}

func (id Identity) tags(role string, routeScoped, includeVersion bool) ([]string, error) {
	if strings.TrimSpace(role) == "" {
		return nil, fmt.Errorf("resource role must not be empty")
	}
	tags := []string{
		identityPrefix + "managed=true",
		tag("cluster", id.value.ClusterID),
		tag("controller", id.value.Controller),
		tag("namespace", id.value.GatewayNamespace),
		tag("name", id.value.GatewayName),
		tag("uid", id.value.GatewayUID),
		tag("role", role),
	}
	if id.value.OpenStackProjectID != "" {
		tags = append(tags, tag("project", id.value.OpenStackProjectID))
	}
	if routeScoped {
		tags = append(tags,
			tag("route-namespace", id.value.RouteNamespace),
			tag("route-name", id.value.RouteName),
			tag("route-uid", id.value.RouteUID),
		)
	}
	if includeVersion && id.value.ControllerVersion != "" {
		tags = append(tags, tag("controller-version", id.value.ControllerVersion))
	}
	return validateTagLengths(tags)
}

func validateTagLengths(tags []string) ([]string, error) {
	for _, value := range tags {
		if len(value) > maxTagLength {
			return nil, fmt.Errorf("encoded identity tag exceeds %d bytes", maxTagLength)
		}
	}
	return tags, nil
}

func containsAll(actual, expected []string) bool {
	actualByKey := make(map[string][]string, len(actual))
	for _, value := range actual {
		key, encodedValue, ok := parseIdentityTag(value)
		if !ok {
			continue
		}
		actualByKey[key] = append(actualByKey[key], encodedValue)
	}
	for _, value := range expected {
		key, encodedValue, ok := parseIdentityTag(value)
		if !ok {
			return false
		}
		values := actualByKey[key]
		if len(values) != 1 || values[0] != encodedValue {
			return false
		}
	}
	return true
}

func tag(key, value string) string {
	prefix := identityPrefix + key + "="
	encoded := encode(value)
	if len(prefix)+len(encoded) > maxTagLength {
		digest := sha256.Sum256([]byte(value))
		encoded = "sha256-" + base64.RawURLEncoding.EncodeToString(digest[:])
	}
	return prefix + encoded
}

func parseIdentityTag(value string) (string, string, bool) {
	if !strings.HasPrefix(value, identityPrefix) {
		return "", "", false
	}
	key, encodedValue, ok := strings.Cut(strings.TrimPrefix(value, identityPrefix), "=")
	if !ok || key == "" {
		return "", "", false
	}
	return key, encodedValue, true
}

func encode(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}
