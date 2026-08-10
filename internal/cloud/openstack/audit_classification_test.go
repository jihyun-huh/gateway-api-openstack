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
	"strings"
	"testing"

	"github.com/jihyun-huh/gateway-api-openstack/internal/audit"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

func TestClassifyAuditCandidates(t *testing.T) {
	identity := auditTestCloudIdentity()
	wrapped, err := NewIdentity(identity)
	if err != nil {
		t.Fatalf("NewIdentity() error = %v", err)
	}
	scopeTags := auditTestScopeTags(identity)
	gatewayRecord := preparedAuditRecord{
		record: audit.OwnershipRecord{
			Identity: identity,
			Objects: []audit.ObjectReference{{
				APIVersion: "gateway.networking.k8s.io/v1", Kind: "Gateway",
				Namespace: identity.GatewayNamespace, Name: identity.GatewayName, UID: identity.GatewayUID,
			}},
		},
		identity: wrapped,
	}

	t.Run("orphan candidate", func(t *testing.T) {
		candidate := auditTestLoadBalancerCandidate(t, identity, "load-balancer-1")
		finding := singleClassifiedAuditFinding(t, candidate, nil, scopeTags, identity.OpenStackProjectID)
		if finding.Disposition != audit.DispositionOrphanCandidate || finding.Reason != auditReasonNoBinding {
			t.Fatalf("finding = %#v, want orphan candidate", finding)
		}
		if finding.Objects == nil {
			t.Fatal("finding objects = nil, want an empty array")
		}
	})

	t.Run("stale Gateway UID", func(t *testing.T) {
		stale := identity
		stale.GatewayUID = "old-gateway-uid"
		candidate := auditTestLoadBalancerCandidate(t, stale, "load-balancer-1")
		finding := singleClassifiedAuditFinding(t, candidate, []preparedAuditRecord{gatewayRecord}, scopeTags, identity.OpenStackProjectID)
		if finding.Disposition != audit.DispositionStaleUID || finding.Reason != auditReasonStaleUID {
			t.Fatalf("finding = %#v, want stale UID", finding)
		}
		if len(finding.Objects) != 1 || finding.Objects[0].UID != identity.GatewayUID {
			t.Fatalf("finding objects = %#v, want current Gateway", finding.Objects)
		}
	})

	t.Run("missing immutable tag", func(t *testing.T) {
		candidate := auditTestLoadBalancerCandidate(t, identity, "load-balancer-1")
		candidate.tags = removeAuditTag(candidate.tags, "uid")
		finding := singleClassifiedAuditFinding(t, candidate, []preparedAuditRecord{gatewayRecord}, scopeTags, identity.OpenStackProjectID)
		if finding.Disposition != audit.DispositionOwnershipConflict || finding.Reason != auditReasonInvalidIdentity {
			t.Fatalf("finding = %#v, want identity conflict", finding)
		}
	})

	t.Run("project identity tag differs from scope", func(t *testing.T) {
		foreignProject := identity
		foreignProject.OpenStackProjectID = "project-b"
		candidate := auditTestLoadBalancerCandidate(t, foreignProject, "load-balancer-1")
		candidate.projectID = identity.OpenStackProjectID
		finding := singleClassifiedAuditFinding(t, candidate, nil, scopeTags, identity.OpenStackProjectID)
		if finding.Disposition != audit.DispositionOwnershipConflict || finding.Reason != auditReasonProjectMismatch {
			t.Fatalf("finding = %#v, want project identity conflict", finding)
		}
	})

	t.Run("malformed encoded tag", func(t *testing.T) {
		candidate := auditTestLoadBalancerCandidate(t, identity, "load-balancer-1")
		candidate.tags = replaceAuditTag(candidate.tags, "name", "%%%")
		finding := singleClassifiedAuditFinding(t, candidate, nil, scopeTags, identity.OpenStackProjectID)
		if finding.Disposition != audit.DispositionOwnershipConflict || finding.Reason != auditReasonInvalidIdentity {
			t.Fatalf("finding = %#v, want malformed identity conflict", finding)
		}
	})

	t.Run("role mismatch does not expose tag value", func(t *testing.T) {
		const privateRole = "private-foreign-role"
		candidate := auditTestLoadBalancerCandidate(t, identity, "load-balancer-1")
		candidate.tags = replaceAuditTag(candidate.tags, "role", encode(privateRole))
		finding := singleClassifiedAuditFinding(t, candidate, nil, scopeTags, identity.OpenStackProjectID)
		if finding.Disposition != audit.DispositionOwnershipConflict || finding.Reason != auditReasonInvalidIdentity {
			t.Fatalf("finding = %#v, want role identity conflict", finding)
		}
		if strings.Contains(finding.Message, privateRole) {
			t.Fatalf("finding message exposes role tag: %q", finding.Message)
		}
	})

	t.Run("route binding does not account for Gateway root", func(t *testing.T) {
		routeIdentity := identity
		routeIdentity.RouteNamespace = "app"
		routeIdentity.RouteName = "route"
		routeIdentity.RouteUID = "route-uid"
		routeWrapped, err := NewIdentity(routeIdentity)
		if err != nil {
			t.Fatalf("NewIdentity() error = %v", err)
		}
		routeRecord := preparedAuditRecord{
			record: audit.OwnershipRecord{
				Identity: routeIdentity,
				Objects: []audit.ObjectReference{{
					APIVersion: "gateway.networking.k8s.io/v1", Kind: "HTTPRoute",
					Namespace: "app", Name: "route", UID: "route-uid",
				}},
			},
			identity: routeWrapped,
			isRoute:  true,
		}
		candidate := auditTestLoadBalancerCandidate(t, identity, "load-balancer-1")
		finding := singleClassifiedAuditFinding(t, candidate, []preparedAuditRecord{routeRecord}, scopeTags, identity.OpenStackProjectID)
		if finding.Disposition != audit.DispositionUnresolved || finding.Reason != auditReasonMissingGatewayBinding {
			t.Fatalf("finding = %#v, want missing Gateway binding", finding)
		}
	})
}

func TestClassifyAuditCandidatesValidatesGraphAndCardinality(t *testing.T) {
	identity := auditTestCloudIdentity()
	wrapped, err := NewIdentity(identity)
	if err != nil {
		t.Fatalf("NewIdentity() error = %v", err)
	}
	record := preparedAuditRecord{
		record: audit.OwnershipRecord{
			Identity: identity,
			Objects:  []audit.ObjectReference{{Kind: "Gateway", Namespace: identity.GatewayNamespace, Name: identity.GatewayName, UID: identity.GatewayUID}},
		},
		identity: wrapped,
	}
	scopeTags := auditTestScopeTags(identity)

	t.Run("reported parent differs from observed parent", func(t *testing.T) {
		listenerTags, err := wrapped.GatewayTags(roleListener)
		if err != nil {
			t.Fatalf("GatewayTags() error = %v", err)
		}
		candidate := auditCandidate{
			service: auditServiceOctavia, resourceType: auditTypeListener,
			id: "listener-1", projectID: identity.OpenStackProjectID, tags: listenerTags,
			reportedParents: singletonStringSet("load-balancer-2"),
			observedParents: singletonStringSet("load-balancer-1"),
		}
		finding := singleClassifiedAuditFinding(t, candidate, []preparedAuditRecord{record}, scopeTags, identity.OpenStackProjectID)
		if finding.Disposition != audit.DispositionOwnershipConflict || finding.Reason != auditReasonParentMismatch {
			t.Fatalf("finding = %#v, want parent conflict", finding)
		}
	})

	t.Run("matched child without parent", func(t *testing.T) {
		listenerTags, err := wrapped.GatewayTags(roleListener)
		if err != nil {
			t.Fatalf("GatewayTags() error = %v", err)
		}
		candidate := auditCandidate{
			service: auditServiceOctavia, resourceType: auditTypeListener,
			id: "listener-1", projectID: identity.OpenStackProjectID, tags: listenerTags,
		}
		finding := singleClassifiedAuditFinding(t, candidate, []preparedAuditRecord{record}, scopeTags, identity.OpenStackProjectID)
		if finding.Disposition != audit.DispositionUnresolved || finding.Reason != auditReasonMissingParent {
			t.Fatalf("finding = %#v, want missing parent", finding)
		}
	})

	t.Run("parent scoped observation is missing", func(t *testing.T) {
		listenerTags, err := wrapped.GatewayTags(roleListener)
		if err != nil {
			t.Fatalf("GatewayTags() error = %v", err)
		}
		loadBalancer := auditTestLoadBalancerCandidate(t, identity, "load-balancer-1")
		listener := auditCandidate{
			service: auditServiceOctavia, resourceType: auditTypeListener,
			id: "listener-1", projectID: identity.OpenStackProjectID, tags: listenerTags,
			reportedParents: singletonStringSet("load-balancer-1"),
		}
		candidates := auditCandidateSet{}
		candidates.add(loadBalancer)
		candidates.add(listener)
		findings := classifyAuditCandidates(candidates, []preparedAuditRecord{record}, scopeTags, identity.OpenStackProjectID)
		finding := auditFindingByID(t, findings, "listener-1")
		if finding.Disposition != audit.DispositionUnresolved || finding.Reason != auditReasonParentNotObserved {
			t.Fatalf("finding = %#v, want missing parent observation", finding)
		}
	})

	t.Run("parent identity differs", func(t *testing.T) {
		listenerTags, err := wrapped.GatewayTags(roleListener)
		if err != nil {
			t.Fatalf("GatewayTags() error = %v", err)
		}
		otherGateway := identity
		otherGateway.GatewayName = "other-gateway"
		otherGateway.GatewayUID = "other-gateway-uid"
		loadBalancer := auditTestLoadBalancerCandidate(t, otherGateway, "load-balancer-1")
		listener := auditCandidate{
			service: auditServiceOctavia, resourceType: auditTypeListener,
			id: "listener-1", projectID: identity.OpenStackProjectID, tags: listenerTags,
			reportedParents: singletonStringSet("load-balancer-1"),
			observedParents: singletonStringSet("load-balancer-1"),
		}
		candidates := auditCandidateSet{}
		candidates.add(loadBalancer)
		candidates.add(listener)
		findings := classifyAuditCandidates(candidates, []preparedAuditRecord{record}, scopeTags, identity.OpenStackProjectID)
		finding := auditFindingByID(t, findings, "listener-1")
		if finding.Disposition != audit.DispositionOwnershipConflict || finding.Reason != auditReasonParentMismatch {
			t.Fatalf("finding = %#v, want parent identity conflict", finding)
		}
	})

	t.Run("duplicate logical roots", func(t *testing.T) {
		first := auditTestLoadBalancerCandidate(t, identity, "load-balancer-2")
		second := auditTestLoadBalancerCandidate(t, identity, "load-balancer-1")
		candidates := auditCandidateSet{}
		candidates.add(first)
		candidates.add(second)
		findings := classifyAuditCandidates(candidates, []preparedAuditRecord{record}, scopeTags, identity.OpenStackProjectID)
		if len(findings) != 2 || findings[0].ID != "load-balancer-1" || findings[1].ID != "load-balancer-2" {
			t.Fatalf("findings = %#v, want deterministic ID order", findings)
		}
		for _, finding := range findings {
			if finding.Disposition != audit.DispositionOwnershipConflict || finding.Reason != auditReasonDuplicateIdentity {
				t.Fatalf("finding = %#v, want duplicate identity conflict", finding)
			}
		}
	})
}

func TestClassifyAuditFloatingIPDoesNotExposeDescription(t *testing.T) {
	identity := auditTestCloudIdentity()
	wrapped, err := NewIdentity(identity)
	if err != nil {
		t.Fatalf("NewIdentity() error = %v", err)
	}
	description := wrapped.GatewayDescription(roleFloatingIP)
	candidate := auditCandidate{
		service: auditServiceNeutron, resourceType: auditTypeFloatingIP,
		id: "floating-ip-1", projectID: identity.OpenStackProjectID,
		description: description,
	}
	finding := singleClassifiedAuditFinding(t, candidate, nil, auditTestScopeTags(identity), identity.OpenStackProjectID)
	if finding.Disposition != audit.DispositionUnresolved || finding.Reason != auditReasonUnattributed {
		t.Fatalf("finding = %#v, want unattributed description", finding)
	}
	if stringsContainAny(finding.Message, description, "identity-sha256") {
		t.Fatalf("finding message exposes description: %q", finding.Message)
	}
}

func TestClassifyAuditFloatingIPRejectsSharedVIPPort(t *testing.T) {
	identity := auditTestCloudIdentity()
	wrapped, err := NewIdentity(identity)
	if err != nil {
		t.Fatalf("NewIdentity() error = %v", err)
	}
	candidate := auditCandidate{
		service: auditServiceNeutron, resourceType: auditTypeFloatingIP,
		id: "floating-ip-1", projectID: identity.OpenStackProjectID,
		description: wrapped.GatewayDescription(roleFloatingIP),
		reportedParents: map[string]struct{}{
			"load-balancer-1": {},
			"load-balancer-2": {},
		},
	}
	finding := singleClassifiedAuditFinding(t, candidate, nil, auditTestScopeTags(identity), identity.OpenStackProjectID)
	if finding.Disposition != audit.DispositionOwnershipConflict || finding.Reason != auditReasonParentMismatch {
		t.Fatalf("finding = %#v, want shared VIP port conflict", finding)
	}
}

func singleClassifiedAuditFinding(
	t *testing.T,
	candidate auditCandidate,
	records []preparedAuditRecord,
	scopeTags []string,
	projectID string,
) audit.ResourceFinding {
	t.Helper()
	candidates := auditCandidateSet{}
	candidates.add(candidate)
	findings := classifyAuditCandidates(candidates, records, scopeTags, projectID)
	if len(findings) != 1 {
		t.Fatalf("classifyAuditCandidates() length = %d, want 1", len(findings))
	}
	return findings[0]
}

func auditFindingByID(t *testing.T, findings []audit.ResourceFinding, id string) audit.ResourceFinding {
	t.Helper()
	for _, finding := range findings {
		if finding.ID == id {
			return finding
		}
	}
	t.Fatalf("finding %q is missing from %#v", id, findings)
	return audit.ResourceFinding{}
}

func auditTestLoadBalancerCandidate(t *testing.T, identity cloud.Identity, id string) auditCandidate {
	t.Helper()
	wrapped, err := NewIdentity(identity)
	if err != nil {
		t.Fatalf("NewIdentity() error = %v", err)
	}
	tags, err := wrapped.GatewayTags(roleLoadBalancer)
	if err != nil {
		t.Fatalf("GatewayTags() error = %v", err)
	}
	return auditCandidate{
		service: auditServiceOctavia, resourceType: auditTypeLoadBalancer,
		id: id, projectID: identity.OpenStackProjectID, provider: "amphora", tags: tags,
	}
}

func auditTestCloudIdentity() cloud.Identity {
	return cloud.Identity{
		OpenStackProjectID: "project-a",
		ClusterID:          "cluster-a",
		Controller:         "example.com/gateway-api-openstack",
		ControllerVersion:  "v0.1.0",
		GatewayNamespace:   "gateway-system",
		GatewayName:        "gateway",
		GatewayUID:         "gateway-uid",
	}
}

func auditTestScopeTags(identity cloud.Identity) []string {
	return []string{
		identityPrefix + "managed=true",
		tag("cluster", identity.ClusterID),
		tag("controller", identity.Controller),
	}
}

func removeAuditTag(tags []string, key string) []string {
	result := make([]string, 0, len(tags))
	for _, value := range tags {
		actualKey, _, ok := parseIdentityTag(value)
		if ok && actualKey == key {
			continue
		}
		result = append(result, value)
	}
	return result
}

func replaceAuditTag(tags []string, key, encodedValue string) []string {
	result := append([]string(nil), tags...)
	for index, value := range result {
		actualKey, _, ok := parseIdentityTag(value)
		if ok && actualKey == key {
			result[index] = identityPrefix + key + "=" + encodedValue
			return result
		}
	}
	return append(result, identityPrefix+key+"="+encodedValue)
}

func stringsContainAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if candidate != "" && strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
