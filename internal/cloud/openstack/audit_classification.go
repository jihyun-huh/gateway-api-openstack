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
	"encoding/base64"
	"fmt"
	"slices"
	"strings"

	"github.com/jihyun-huh/gateway-api-openstack/internal/audit"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

const (
	auditReasonExactBinding          = "ExactBinding"
	auditReasonNoBinding             = "NoBinding"
	auditReasonStaleUID              = "StaleUID"
	auditReasonInvalidIdentity       = "InvalidIdentity"
	auditReasonProjectMismatch       = "ProjectMismatch"
	auditReasonProjectNotReported    = "ProjectNotReported"
	auditReasonParentMismatch        = "ParentMismatch"
	auditReasonMissingParent         = "MissingParent"
	auditReasonMissingGatewayBinding = "MissingGatewayBinding"
	auditReasonObservationChanged    = "ObservationChanged"
	auditReasonParentNotObserved     = "ParentNotObserved"
	auditReasonDuplicateIdentity     = "DuplicateIdentity"
	auditReasonDetachedFloatingIP    = "DetachedFloatingIP"
	auditReasonUnattributed          = "UnattributedDescription"
	auditReasonProviderNotReported   = "ProviderNotReported"
	auditReasonMissingBackendPool    = "MissingBackendPool"
	auditReasonParentConflict        = "ParentConflict"
	auditReasonParentUnresolved      = "ParentUnresolved"
)

type classifiedAuditCandidate struct {
	candidate *auditCandidate
	finding   audit.ResourceFinding
}

func classifyAuditCandidates(
	candidates auditCandidateSet,
	records []preparedAuditRecord,
	scopeTags []string,
	scopeProjectID string,
) []audit.ResourceFinding {
	classified := make([]classifiedAuditCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		classified = append(classified, classifiedAuditCandidate{
			candidate: candidate,
			finding:   classifyAuditCandidate(candidates, candidate, records, scopeTags, scopeProjectID),
		})
	}
	markDuplicateAuditIdentities(classified)
	propagateAuditParentFindings(classified)
	slices.SortFunc(classified, compareClassifiedAuditCandidates)
	findings := make([]audit.ResourceFinding, 0, len(classified))
	for _, item := range classified {
		item.finding.Objects = audit.SortObjectReferences(item.finding.Objects)
		if item.finding.Objects == nil {
			item.finding.Objects = []audit.ObjectReference{}
		}
		findings = append(findings, item.finding)
	}
	return findings
}

func classifyAuditCandidate(
	candidates auditCandidateSet,
	candidate *auditCandidate,
	records []preparedAuditRecord,
	scopeTags []string,
	scopeProjectID string,
) audit.ResourceFinding {
	parentID, parentReason := auditCandidateParent(candidate)
	finding := audit.ResourceFinding{
		Service: candidate.service, Type: candidate.resourceType,
		ID: candidate.id, ParentID: parentID,
		ProvisioningStatus: candidate.provisioningStatus,
		Objects:            []audit.ObjectReference{},
	}
	if strings.TrimSpace(candidate.id) == "" {
		return auditFinding(finding, audit.DispositionOwnershipConflict, auditReasonInvalidIdentity,
			"OpenStack resource does not report an ID")
	}
	if candidate.observationChanged {
		return auditFinding(finding, audit.DispositionUnresolved, auditReasonObservationChanged,
			"Resource identity or relationship changed while the inventory was being collected")
	}
	resourceProjectID := candidate.projectID
	if resourceProjectID == "" {
		resourceProjectID = candidate.tenantID
	}
	if resourceProjectID == "" {
		return auditFinding(finding, audit.DispositionUnresolved, auditReasonProjectNotReported,
			"OpenStack did not report the resource project")
	}
	if resourceProjectID != scopeProjectID {
		return auditFinding(finding, audit.DispositionOwnershipConflict, auditReasonProjectMismatch,
			"Resource project differs from the authenticated audit project")
	}
	if parentReason != "" {
		return auditFinding(finding, audit.DispositionOwnershipConflict, auditReasonParentMismatch, parentReason)
	}
	if candidate.resourceType == auditTypeFloatingIP {
		finding.Role = roleFloatingIP
		return classifyAuditFloatingIP(candidates, candidate, finding, records)
	}
	if !containsAll(candidate.tags, scopeTags) {
		return auditFinding(finding, audit.DispositionOwnershipConflict, auditReasonInvalidIdentity,
			"Resource was discovered in a managed graph but does not carry the controller scope identity")
	}

	role, routeScoped, err := auditCandidateRole(candidate.resourceType, candidate.tags)
	if err != nil {
		return auditFinding(finding, audit.DispositionOwnershipConflict, auditReasonInvalidIdentity, err.Error())
	}
	finding.Role = role
	if err := validateAuditIdentityTags(candidate.tags, routeScoped); err != nil {
		return auditFinding(finding, audit.DispositionOwnershipConflict, auditReasonInvalidIdentity, err.Error())
	}
	if !containsAll(candidate.tags, []string{tag("project", scopeProjectID)}) {
		return auditFinding(finding, audit.DispositionOwnershipConflict, auditReasonProjectMismatch,
			"Resource identity tag does not match the authenticated audit project")
	}
	exact, discovery, routeGateway := matchingAuditRecords(records, candidate.tags, role, routeScoped)
	switch {
	case len(exact) != 0:
		finding.Objects = auditObjects(exact)
	case len(routeGateway) != 0:
		finding.Objects = auditObjects(routeGateway)
	case len(discovery) != 0:
		finding.Objects = auditObjects(discovery)
	}
	if candidate.resourceType == auditTypeLoadBalancer {
		if candidate.provider == "" {
			return auditFinding(finding, audit.DispositionUnresolved, auditReasonProviderNotReported,
				"OpenStack did not report the load balancer provider")
		}
		if candidate.provider != "amphora" {
			return auditFinding(finding, audit.DispositionOwnershipConflict, auditReasonInvalidIdentity,
				"Managed load balancer does not use the Amphora provider")
		}
	}
	if parentID != "" && auditResourceRequiresParentObservation(candidate.resourceType) &&
		auditParentExists(candidates, candidate.resourceType, parentID) && len(candidate.observedParents) == 0 {
		return auditFinding(finding, audit.DispositionUnresolved, auditReasonParentNotObserved,
			"Resource reported a scoped parent but was absent from the parent-scoped observation")
	}
	if parentID != "" {
		if err := validateAuditParentIdentity(candidates, candidate, parentID); err != nil {
			return auditFinding(finding, audit.DispositionOwnershipConflict, auditReasonParentMismatch, err.Error())
		}
	}
	if candidate.resourceType == auditTypeL7Policy {
		if candidate.redirectPoolID == "" {
			return auditFinding(finding, audit.DispositionUnresolved, auditReasonMissingBackendPool,
				"L7 policy does not report its redirect pool")
		}
		if !auditCandidateExists(candidates, auditTypePool, candidate.redirectPoolID) {
			return auditFinding(finding, audit.DispositionUnresolved, auditReasonMissingBackendPool,
				"L7 policy redirect pool is not present in the scoped inventory")
		}
		if err := validateAuditPolicyPool(candidates, candidate, candidate.redirectPoolID); err != nil {
			return auditFinding(finding, audit.DispositionOwnershipConflict, auditReasonParentMismatch, err.Error())
		}
	}

	switch {
	case len(exact) > 1:
		finding.Objects = auditObjects(exact)
		return auditFinding(finding, audit.DispositionOwnershipConflict, auditReasonInvalidIdentity,
			"More than one durable Kubernetes binding exactly matches this resource")
	case len(exact) == 1:
		finding.Objects = auditObjects(exact)
		finding = auditFinding(finding, audit.DispositionMatched, auditReasonExactBinding,
			"Resource matches a current Kubernetes ownership binding")
	case !routeScoped && len(routeGateway) != 0:
		finding.Objects = auditObjects(routeGateway)
		finding = auditFinding(finding, audit.DispositionUnresolved, auditReasonMissingGatewayBinding,
			"An HTTPRoute binding refers to this Gateway identity, but no durable Gateway binding matches it")
	case len(discovery) > 1:
		finding.Objects = auditObjects(discovery)
		return auditFinding(finding, audit.DispositionOwnershipConflict, auditReasonInvalidIdentity,
			"Stable Kubernetes object names match more than one durable binding")
	case len(discovery) == 1:
		finding.Objects = auditObjects(discovery)
		if onlyAuditUIDsDiffer(candidate.tags, discovery[0], role, routeScoped) {
			finding = auditFinding(finding, audit.DispositionStaleUID, auditReasonStaleUID,
				"Resource names match a current binding, but a Kubernetes UID differs")
		} else {
			finding = auditFinding(finding, audit.DispositionOwnershipConflict, auditReasonInvalidIdentity,
				"Stable object names match a current binding, but another immutable identity field differs")
		}
	default:
		finding = auditFinding(finding, audit.DispositionOrphanCandidate, auditReasonNoBinding,
			"Resource belongs to this controller scope, but no current durable binding matches it")
	}

	if finding.Disposition == audit.DispositionMatched && auditResourceNeedsParent(candidate.resourceType) {
		if parentID == "" || !auditParentExists(candidates, candidate.resourceType, parentID) {
			return auditFinding(finding, audit.DispositionUnresolved, auditReasonMissingParent,
				"Resource matches a binding, but its expected parent is not present in the scoped inventory")
		}
	}
	return finding
}

func classifyAuditFloatingIP(
	candidates auditCandidateSet,
	candidate *auditCandidate,
	finding audit.ResourceFinding,
	records []preparedAuditRecord,
) audit.ResourceFinding {
	if !strings.HasPrefix(candidate.description, "gateway-api-openstack; identity-sha256=") {
		return auditFinding(finding, audit.DispositionOwnershipConflict, auditReasonInvalidIdentity,
			"Floating IP attached to a managed VIP port does not carry the expected ownership description")
	}
	exactGateway := make([]preparedAuditRecord, 0, 1)
	routeGateway := make([]preparedAuditRecord, 0, 1)
	for _, record := range records {
		if !record.identity.MatchesGatewayDescription(candidate.description, roleFloatingIP) {
			continue
		}
		if record.isRoute {
			routeGateway = appendUniqueAuditRecord(routeGateway, record)
		} else {
			exactGateway = appendUniqueAuditRecord(exactGateway, record)
		}
	}
	parentID, _ := auditCandidateParent(candidate)
	switch {
	case len(exactGateway) > 1:
		finding.Objects = auditObjects(exactGateway)
		return auditFinding(finding, audit.DispositionOwnershipConflict, auditReasonInvalidIdentity,
			"More than one durable Gateway binding matches this Floating IP")
	case len(exactGateway) == 1 && parentID == "":
		finding.Objects = auditObjects(exactGateway)
		return auditFinding(finding, audit.DispositionUnresolved, auditReasonDetachedFloatingIP,
			"Floating IP matches a Gateway binding but is detached from the scoped load balancer VIP port")
	case len(exactGateway) == 1:
		finding.Objects = auditObjects(exactGateway)
		if !auditParentExists(candidates, auditTypeFloatingIP, parentID) {
			return auditFinding(finding, audit.DispositionUnresolved, auditReasonMissingParent,
				"Floating IP matches a binding, but its load balancer is not present in the scoped inventory")
		}
		parent := auditParentCandidate(candidates, auditTypeFloatingIP, parentID)
		if parent == nil || !exactGateway[0].identity.MatchesGateway(parent.tags, roleLoadBalancer) {
			return auditFinding(finding, audit.DispositionOwnershipConflict, auditReasonParentMismatch,
				"Floating IP binding does not match the identity of its attached load balancer")
		}
		return auditFinding(finding, audit.DispositionMatched, auditReasonExactBinding,
			"Floating IP matches a current Gateway ownership binding and VIP port")
	case len(routeGateway) != 0:
		finding.Objects = auditObjects(routeGateway)
		if parentID != "" {
			parent := auditParentCandidate(candidates, auditTypeFloatingIP, parentID)
			if parent == nil || !routeGateway[0].identity.MatchesGateway(parent.tags, roleLoadBalancer) {
				return auditFinding(finding, audit.DispositionOwnershipConflict, auditReasonParentMismatch,
					"Floating IP route binding does not match the identity of its attached load balancer")
			}
		}
		return auditFinding(finding, audit.DispositionUnresolved, auditReasonMissingGatewayBinding,
			"An HTTPRoute binding refers to this Floating IP identity, but no durable Gateway binding matches it")
	case parentID != "":
		return auditFinding(finding, audit.DispositionUnresolved, auditReasonUnattributed,
			"Floating IP is attached to a scoped load balancer, but its description digest does not match a current Gateway binding")
	default:
		return auditFinding(finding, audit.DispositionUnresolved, auditReasonUnattributed,
			"Detached Floating IP has a controller description whose project, cluster, and object identity cannot be recovered")
	}
}

func auditFinding(
	finding audit.ResourceFinding,
	disposition audit.Disposition,
	reason string,
	message string,
) audit.ResourceFinding {
	finding.Disposition = disposition
	finding.Reason = reason
	finding.Message = message
	return finding
}

func auditCandidateParent(candidate *auditCandidate) (string, string) {
	reported := sortedSetValues(candidate.reportedParents)
	observed := sortedSetValues(candidate.observedParents)
	if len(reported) > 1 || len(observed) > 1 {
		return "", "Resource reports more than one parent in a graph that requires one"
	}
	if len(reported) == 1 && len(observed) == 1 && reported[0] != observed[0] {
		return "", "Resource reports a different parent from the collection in which it was observed"
	}
	if len(observed) == 1 {
		return observed[0], ""
	}
	if len(reported) == 1 {
		return reported[0], ""
	}
	return "", ""
}

func auditCandidateRole(resourceType string, tags []string) (string, bool, error) {
	encodedRole, ok := singleAuditTagValue(tags, "role")
	if !ok {
		return "", false, fmt.Errorf("resource has a missing or duplicate role identity tag")
	}
	decodedRole, err := base64.RawURLEncoding.DecodeString(encodedRole)
	if err != nil {
		return "", false, fmt.Errorf("resource has an invalid role identity tag")
	}
	role := string(decodedRole)
	routeScoped := false
	valid := false
	switch resourceType {
	case auditTypeLoadBalancer:
		valid = role == roleLoadBalancer
	case auditTypeListener:
		valid = role == roleListener
	case auditTypePool:
		valid, routeScoped = role == rolePool, true
	case auditTypeMember:
		valid, routeScoped = role == roleMember, true
	case auditTypeMonitor:
		valid, routeScoped = role == roleMonitor, true
	case auditTypeL7Policy:
		valid, routeScoped = role == rolePolicyExact || role == rolePolicyPrefix, true
	case auditTypeL7Rule:
		valid, routeScoped = role == roleRulePath || role == roleRuleHost, true
	}
	if !valid {
		return "", false, fmt.Errorf("resource role does not match its OpenStack type")
	}
	return role, routeScoped, nil
}

func validateAuditIdentityTags(tags []string, routeScoped bool) error {
	required := []string{"managed", "cluster", "controller", "project", "namespace", "name", "uid", "role"}
	if routeScoped {
		required = append(required, "route-namespace", "route-name", "route-uid")
	}
	for _, key := range required {
		value, ok := singleAuditTagValue(tags, key)
		if !ok || value == "" {
			return fmt.Errorf("resource has a missing or duplicate %s identity tag", key)
		}
		if key == "managed" && value != "true" {
			return fmt.Errorf("resource managed identity tag is not true")
		}
		if key != "managed" && !validAuditEncodedValue(value) {
			return fmt.Errorf("resource has an invalid %s identity tag", key)
		}
	}
	return nil
}

func validAuditEncodedValue(value string) bool {
	if digest, ok := strings.CutPrefix(value, "sha256-"); ok {
		decoded, err := base64.RawURLEncoding.DecodeString(digest)
		return err == nil && len(decoded) == 32
	}
	_, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil
}

func singleAuditTagValue(tags []string, wantedKey string) (string, bool) {
	found := ""
	count := 0
	for _, value := range tags {
		key, encodedValue, ok := parseIdentityTag(value)
		if ok && key == wantedKey {
			found = encodedValue
			count++
		}
	}
	return found, count == 1
}

func matchingAuditRecords(
	records []preparedAuditRecord,
	tags []string,
	role string,
	routeScoped bool,
) (exact, discovery, routeGateway []preparedAuditRecord) {
	for _, record := range records {
		if routeScoped {
			if !record.isRoute {
				continue
			}
			if record.identity.MatchesRoute(tags, role) {
				exact = appendUniqueAuditRecord(exact, record)
			} else if record.identity.MatchesRouteDiscovery(tags, role) {
				discovery = appendUniqueAuditRecord(discovery, record)
			}
			continue
		}
		if record.isRoute {
			if record.identity.MatchesGateway(tags, role) {
				routeGateway = appendUniqueAuditRecord(routeGateway, record)
			}
			continue
		}
		if record.identity.MatchesGateway(tags, role) {
			exact = appendUniqueAuditRecord(exact, record)
		} else if record.identity.MatchesGatewayDiscovery(tags, role) {
			discovery = appendUniqueAuditRecord(discovery, record)
		}
	}
	return exact, discovery, routeGateway
}

func appendUniqueAuditRecord(records []preparedAuditRecord, candidate preparedAuditRecord) []preparedAuditRecord {
	for index := range records {
		if sameAuditOwnershipIdentity(records[index].record.Identity, candidate.record.Identity) {
			records[index].record.Objects = audit.SortObjectReferences(append(records[index].record.Objects, candidate.record.Objects...))
			return records
		}
	}
	return append(records, candidate)
}

func sameAuditOwnershipIdentity(left, right cloud.Identity) bool {
	left.ControllerVersion = ""
	right.ControllerVersion = ""
	return left == right
}

func auditObjects(records []preparedAuditRecord) []audit.ObjectReference {
	objects := make([]audit.ObjectReference, 0)
	for _, record := range records {
		objects = append(objects, record.record.Objects...)
	}
	return audit.SortObjectReferences(objects)
}

func onlyAuditUIDsDiffer(tags []string, record preparedAuditRecord, role string, routeScoped bool) bool {
	var expectedTags []string
	var err error
	if routeScoped {
		expectedTags, err = record.identity.RouteTags(role)
	} else {
		expectedTags, err = record.identity.GatewayTags(role)
	}
	if err != nil {
		return false
	}
	actual := uniqueAuditTags(tags)
	expected := uniqueAuditTags(expectedTags)
	differences := make([]string, 0, 2)
	for _, key := range []string{"managed", "cluster", "controller", "project", "namespace", "name", "uid", "role", "route-namespace", "route-name", "route-uid"} {
		if actual[key] != expected[key] {
			differences = append(differences, key)
		}
	}
	if len(differences) == 0 {
		return false
	}
	for _, key := range differences {
		if key != "uid" && key != "route-uid" {
			return false
		}
	}
	return true
}

func uniqueAuditTags(tags []string) map[string]string {
	result := make(map[string]string)
	duplicates := make(map[string]bool)
	for _, value := range tags {
		key, encodedValue, ok := parseIdentityTag(value)
		if !ok {
			continue
		}
		if _, exists := result[key]; exists {
			duplicates[key] = true
		}
		result[key] = encodedValue
	}
	for key := range duplicates {
		result[key] = ""
	}
	return result
}

func auditResourceNeedsParent(resourceType string) bool {
	switch resourceType {
	case auditTypeListener, auditTypePool, auditTypeMember, auditTypeMonitor, auditTypeL7Policy, auditTypeL7Rule:
		return true
	default:
		return false
	}
}

func auditResourceRequiresParentObservation(resourceType string) bool {
	switch resourceType {
	case auditTypeListener, auditTypePool, auditTypeMonitor:
		return true
	default:
		return false
	}
}

func validateAuditParentIdentity(candidates auditCandidateSet, child *auditCandidate, parentID string) error {
	parent := auditParentCandidate(candidates, child.resourceType, parentID)
	if parent == nil {
		return nil
	}
	keys := []string{"managed", "cluster", "controller", "project", "namespace", "name", "uid"}
	switch child.resourceType {
	case auditTypeMember, auditTypeMonitor, auditTypeL7Rule:
		keys = append(keys, "route-namespace", "route-name", "route-uid")
	}
	if !auditTagValuesEqual(child.tags, parent.tags, keys) {
		return fmt.Errorf("resource identity does not match its scoped parent")
	}
	return nil
}

func validateAuditPolicyPool(candidates auditCandidateSet, policy *auditCandidate, poolID string) error {
	var pool *auditCandidate
	for _, candidate := range candidates {
		if candidate.resourceType == auditTypePool && candidate.id == poolID {
			pool = candidate
			break
		}
	}
	if pool == nil {
		return nil
	}
	keys := []string{
		"managed", "cluster", "controller", "project", "namespace", "name", "uid",
		"route-namespace", "route-name", "route-uid",
	}
	if !auditTagValuesEqual(policy.tags, pool.tags, keys) {
		return fmt.Errorf("policy identity does not match its redirect pool")
	}
	return nil
}

func auditCandidateExists(candidates auditCandidateSet, resourceType, id string) bool {
	for _, candidate := range candidates {
		if candidate.resourceType == resourceType && candidate.id == id {
			return true
		}
	}
	return false
}

func auditTagValuesEqual(left, right []string, keys []string) bool {
	for _, key := range keys {
		leftValue, leftOK := singleAuditTagValue(left, key)
		rightValue, rightOK := singleAuditTagValue(right, key)
		if !leftOK || !rightOK || leftValue != rightValue {
			return false
		}
	}
	return true
}

func auditParentCandidate(candidates auditCandidateSet, childType, parentID string) *auditCandidate {
	parentType := auditParentType(childType)
	for _, candidate := range candidates {
		if candidate.resourceType == parentType && candidate.id == parentID {
			return candidate
		}
	}
	return nil
}

func auditParentType(childType string) string {
	switch childType {
	case auditTypeListener, auditTypePool, auditTypeFloatingIP:
		return auditTypeLoadBalancer
	case auditTypeMember, auditTypeMonitor:
		return auditTypePool
	case auditTypeL7Policy:
		return auditTypeListener
	case auditTypeL7Rule:
		return auditTypeL7Policy
	default:
		return ""
	}
}

func auditParentExists(candidates auditCandidateSet, childType, parentID string) bool {
	return auditParentCandidate(candidates, childType, parentID) != nil
}

func markDuplicateAuditIdentities(classified []classifiedAuditCandidate) {
	groups := make(map[string][]int)
	for index, item := range classified {
		key := auditCardinalityKey(item.candidate, item.finding)
		if key != "" {
			groups[key] = append(groups[key], index)
		}
	}
	for _, indexes := range groups {
		if len(indexes) < 2 {
			continue
		}
		for _, index := range indexes {
			classified[index].finding = auditFinding(
				classified[index].finding,
				audit.DispositionOwnershipConflict,
				auditReasonDuplicateIdentity,
				"More than one OpenStack resource has the same logical controller identity",
			)
		}
	}
}

func propagateAuditParentFindings(classified []classifiedAuditCandidate) {
	byResource := make(map[string]*audit.ResourceFinding, len(classified))
	for index := range classified {
		key := classified[index].finding.Type + "\x00" + classified[index].finding.ID
		byResource[key] = &classified[index].finding
	}
	for order := 1; order <= auditResourceOrder(auditTypeFloatingIP); order++ {
		for index := range classified {
			item := &classified[index]
			if auditResourceOrder(item.finding.Type) != order || item.finding.Disposition != audit.DispositionMatched {
				continue
			}
			parents := []struct {
				resourceType string
				id           string
			}{
				{resourceType: auditParentType(item.finding.Type), id: item.finding.ParentID},
			}
			if item.finding.Type == auditTypeL7Policy {
				parents = append(parents, struct {
					resourceType string
					id           string
				}{resourceType: auditTypePool, id: item.candidate.redirectPoolID})
			}
			for _, parentReference := range parents {
				if parentReference.resourceType == "" || parentReference.id == "" {
					continue
				}
				parent := byResource[parentReference.resourceType+"\x00"+parentReference.id]
				if parent == nil || parent.Disposition == audit.DispositionMatched {
					continue
				}
				if parent.Disposition == audit.DispositionOwnershipConflict {
					item.finding = auditFinding(item.finding, audit.DispositionOwnershipConflict, auditReasonParentConflict,
						"Resource parent has an ownership conflict")
				} else {
					item.finding = auditFinding(item.finding, audit.DispositionUnresolved, auditReasonParentUnresolved,
						"Resource parent is not fully matched to a current binding")
				}
				break
			}
		}
	}
}

func auditCardinalityKey(candidate *auditCandidate, finding audit.ResourceFinding) string {
	if finding.Disposition == audit.DispositionOwnershipConflict || candidate.resourceType == auditTypeMember {
		return ""
	}
	if candidate.resourceType == auditTypeFloatingIP {
		description, _, _ := strings.Cut(candidate.description, "; controller-version=")
		if description == "" {
			return ""
		}
		return candidate.resourceType + "\x00" + description
	}
	stableKeys := []string{"cluster", "controller", "namespace", "name", "role"}
	switch candidate.resourceType {
	case auditTypePool, auditTypeMonitor, auditTypeL7Policy, auditTypeL7Rule:
		stableKeys = append(stableKeys, "route-namespace", "route-name")
	}
	parts := []string{candidate.resourceType}
	for _, key := range stableKeys {
		value, ok := singleAuditTagValue(candidate.tags, key)
		if !ok {
			return ""
		}
		parts = append(parts, value)
	}
	if candidate.resourceType == auditTypeL7Rule {
		parts = append(parts, finding.ParentID)
	}
	return strings.Join(parts, "\x00")
}

func compareClassifiedAuditCandidates(left, right classifiedAuditCandidate) int {
	leftKey := fmt.Sprintf("%s\x00%02d\x00%s\x00%s", left.finding.Service, auditResourceOrder(left.finding.Type), left.finding.ParentID, left.finding.ID)
	rightKey := fmt.Sprintf("%s\x00%02d\x00%s\x00%s", right.finding.Service, auditResourceOrder(right.finding.Type), right.finding.ParentID, right.finding.ID)
	return strings.Compare(leftKey, rightKey)
}

func auditResourceOrder(resourceType string) int {
	switch resourceType {
	case auditTypeLoadBalancer:
		return 0
	case auditTypeListener:
		return 1
	case auditTypePool:
		return 2
	case auditTypeMember:
		return 3
	case auditTypeMonitor:
		return 4
	case auditTypeL7Policy:
		return 5
	case auditTypeL7Rule:
		return 6
	case auditTypeFloatingIP:
		return 7
	default:
		return 99
	}
}
