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

package identity

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

func TestIdentityMatchesProjectButNotControllerVersion(t *testing.T) {
	value := cloud.Identity{
		OpenStackProjectID: "project-a",
		ClusterID:          "cluster-a",
		Controller:         "example.com/openstack",
		ControllerVersion:  "v0.1.0",
		GatewayNamespace:   "default",
		GatewayName:        "edge",
		GatewayUID:         "gateway-uid",
	}
	identity, err := NewIdentity(value)
	if err != nil {
		t.Fatal(err)
	}
	tags, err := identity.GatewayTags(RoleLoadBalancer)
	if err != nil {
		t.Fatal(err)
	}
	if !identity.MatchesGateway(tags, RoleLoadBalancer) {
		t.Fatal("identity did not match its complete resource tags")
	}

	upgradedValue := value
	upgradedValue.ControllerVersion = "v0.1.1"
	upgraded, err := NewIdentity(upgradedValue)
	if err != nil {
		t.Fatal(err)
	}
	if !upgraded.MatchesGateway(tags, RoleLoadBalancer) {
		t.Fatal("controller upgrade changed immutable resource ownership")
	}
	otherProjectValue := value
	otherProjectValue.OpenStackProjectID = "project-b"
	otherProject, err := NewIdentity(otherProjectValue)
	if err != nil {
		t.Fatal(err)
	}
	if otherProject.MatchesGateway(tags, RoleLoadBalancer) {
		t.Fatal("identity accepted resource tags from another OpenStack project")
	}
	discovery, err := identity.GatewayDiscoveryTags(RoleLoadBalancer)
	if err != nil {
		t.Fatal(err)
	}
	otherProjectDiscovery, err := otherProject.GatewayDiscoveryTags(RoleLoadBalancer)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(discovery, otherProjectDiscovery) {
		t.Fatalf("project identity leaked into discovery tags: %#v != %#v", discovery, otherProjectDiscovery)
	}
}

func TestIdentityHashesLongTagValuesAndRejectsConflicts(t *testing.T) {
	value := cloud.Identity{
		OpenStackProjectID: strings.Repeat("p", 300),
		ClusterID:          strings.Repeat("c", 300),
		Controller:         "example.com/" + strings.Repeat("controller", 30),
		ControllerVersion:  strings.Repeat("version", 100),
		GatewayNamespace:   "default",
		GatewayName:        strings.Repeat("gateway", 40),
		GatewayUID:         "gateway-uid",
	}
	identity, err := NewIdentity(value)
	if err != nil {
		t.Fatal(err)
	}
	tags, err := identity.GatewayTags(RoleLoadBalancer)
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range tags {
		if len(tag) > maxTagLength {
			t.Fatalf("identity tag length = %d, want at most %d: %q", len(tag), maxTagLength, tag)
		}
	}
	if !identity.MatchesGateway(tags, RoleLoadBalancer) {
		t.Fatal("identity did not match tags containing hashed values")
	}
	conflicting := append([]string(nil), tags...)
	conflicting = append(conflicting, tag("uid", "another-gateway-uid"))
	if identity.MatchesGateway(conflicting, RoleLoadBalancer) {
		t.Fatal("identity accepted conflicting values for a reserved identity tag")
	}
	if got := len(identity.Description(RoleFloatingIP)); got > maxTagLength {
		t.Fatalf("description length = %d, want at most %d", got, maxTagLength)
	}
}

func TestGatewayDescriptionIgnoresRouteFields(t *testing.T) {
	value := cloud.Identity{
		OpenStackProjectID: "project-a",
		ClusterID:          "cluster-a",
		Controller:         "example.com/openstack",
		ControllerVersion:  "v0.1.0",
		GatewayNamespace:   "default",
		GatewayName:        "edge",
		GatewayUID:         "gateway-uid",
	}
	gatewayIdentity, err := NewIdentity(value)
	if err != nil {
		t.Fatal(err)
	}
	value.RouteNamespace = "default"
	value.RouteName = "api"
	value.RouteUID = "route-uid"
	routeIdentity, err := NewIdentity(value)
	if err != nil {
		t.Fatal(err)
	}
	description := gatewayIdentity.GatewayDescription(RoleFloatingIP)
	if !routeIdentity.MatchesGatewayDescription(description, RoleFloatingIP) {
		t.Fatal("route-scoped identity did not recognize its Gateway-scoped Floating IP description")
	}
}
