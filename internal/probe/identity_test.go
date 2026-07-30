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

import "testing"

func TestIdentityRequiresFullMatch(t *testing.T) {
	t.Parallel()

	identity := Identity{
		ClusterID:        "cluster-a",
		Controller:       controllerIdentifier,
		GatewayNamespace: "default",
		GatewayName:      "web",
		GatewayUID:       "44d21ac1-c54a-4e93-bceb-c2a632bd2f8b",
	}
	tags, err := identity.Tags("loadbalancer")
	if err != nil {
		t.Fatalf("Tags() error = %v", err)
	}
	if !identity.Matches(tags, "loadbalancer") {
		t.Fatal("Matches() rejected the complete identity")
	}
	if identity.Matches(tags, "listener") {
		t.Fatal("Matches() accepted the wrong resource role")
	}

	missingUID := append([]string(nil), tags...)
	missingUID = missingUID[:len(missingUID)-2]
	if identity.Matches(missingUID, "loadbalancer") {
		t.Fatal("Matches() accepted an incomplete identity")
	}
}

func TestIdentityDescriptionsAreRoleSpecific(t *testing.T) {
	t.Parallel()

	identity := Identity{
		ClusterID:        "cluster-a",
		Controller:       controllerIdentifier,
		GatewayNamespace: "default",
		GatewayName:      "web",
		GatewayUID:       "uid",
	}
	description := identity.Description("floating-ip") + "; phase=verified"
	if !identity.MatchesDescription(description, "floating-ip") {
		t.Fatal("MatchesDescription() rejected its identity prefix")
	}
	if identity.MatchesDescription(description, "loadbalancer") {
		t.Fatal("MatchesDescription() accepted the wrong role")
	}

	otherController := identity
	otherController.Controller = "another-controller"
	if otherController.MatchesDescription(description, "floating-ip") {
		t.Fatal("MatchesDescription() accepted a different controller")
	}
}

func TestIdentityDescriptionHasBoundedLength(t *testing.T) {
	t.Parallel()

	identity := Identity{
		ClusterID:        string(make([]byte, 1024)),
		Controller:       controllerIdentifier,
		GatewayNamespace: string(make([]byte, 1024)),
		GatewayName:      string(make([]byte, 1024)),
		GatewayUID:       string(make([]byte, 1024)),
	}
	if got := len(identity.Description("floating-ip")); got > 255 {
		t.Fatalf("description length = %d, want no more than 255", got)
	}
}
