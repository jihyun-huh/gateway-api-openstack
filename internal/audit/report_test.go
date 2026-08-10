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

package audit

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

func TestBuildReport(t *testing.T) {
	generatedAt := time.Date(2026, time.August, 10, 12, 30, 0, 0, time.FixedZone("test", 2*60*60))
	scope := reportTestScope()
	gatewayRecord, routeRecord := reportTestRecords(scope)
	snapshot := KubernetesSnapshot{
		Records: []OwnershipRecord{routeRecord, gatewayRecord},
		Issues: []KubernetesIssue{
			{Object: reportTestObject("HTTPRoute", "zeta", "broken", "broken-route-uid"), Reason: "InvalidBinding", Message: "Controller binding is incomplete"},
			{Object: reportTestObject("Gateway", "alpha", "broken", "broken-gateway-uid"), Reason: "InvalidBinding", Message: "Controller binding is incomplete"},
		},
	}
	inventory := Inventory{
		Resources: []ResourceFinding{
			reportTestFinding("Neutron", "FloatingIP", "fip-1", DispositionUnresolved),
			reportTestFinding("Octavia", "Pool", "pool-1", DispositionStaleUID),
			reportTestFinding("Octavia", "Listener", "listener-1", DispositionMatched),
			reportTestFinding("Octavia", "LoadBalancer", "lb-1", DispositionOrphanCandidate),
			reportTestFinding("Octavia", "L7Policy", "policy-1", DispositionOwnershipConflict),
		},
		Limitations: []string{"Zeta limitation.", "Alpha limitation."},
	}

	report, err := BuildReport(generatedAt, "v0.2.0-test", scope, snapshot, inventory)
	if err != nil {
		t.Fatalf("BuildReport() error = %v", err)
	}
	if report.FormatVersion != ReportFormatVersion || report.ToolVersion != "v0.2.0-test" {
		t.Fatalf("BuildReport() versions = %q, %q", report.FormatVersion, report.ToolVersion)
	}
	if !report.GeneratedAt.Equal(generatedAt) || report.GeneratedAt.Location() != time.UTC {
		t.Fatalf("BuildReport() generatedAt = %v, want same instant in UTC", report.GeneratedAt)
	}
	if report.Scope.ClusterID != scope.ClusterID || report.Scope.ControllerName != scope.ControllerName {
		t.Fatalf("BuildReport() scope = %#v, want cluster and controller", report.Scope)
	}
	if report.Assessment != AssessmentIncomplete {
		t.Fatalf("BuildReport() assessment = %q, want %q", report.Assessment, AssessmentIncomplete)
	}
	wantSummary := Summary{
		KubernetesBindings: 2,
		KubernetesIssues:   2,
		OpenStackResources: 5,
		Matched:            1,
		OrphanCandidates:   1,
		StaleUIDs:          1,
		OwnershipConflicts: 1,
		Unresolved:         1,
	}
	if report.Summary != wantSummary {
		t.Fatalf("BuildReport() summary = %#v, want %#v", report.Summary, wantSummary)
	}
	if !report.HasFindings() {
		t.Fatal("BuildReport().HasFindings() = false, want true")
	}
	if report.KubernetesIssues[0].Object.Kind != "Gateway" {
		t.Fatalf("BuildReport() issues are not sorted: %#v", report.KubernetesIssues)
	}
	if report.Resources[0].Service != "Neutron" || report.Resources[1].ID != "policy-1" {
		t.Fatalf("BuildReport() resources are not sorted: %#v", report.Resources)
	}
	if len(report.Advisories) != 4 || !strings.Contains(report.Advisories[3], "Restore") {
		t.Fatalf("BuildReport() advisories = %#v, want incomplete binding warning", report.Advisories)
	}
	if !slices.IsSorted(report.Limitations) || !slices.Contains(report.Limitations, desiredGraphLimitation) {
		t.Fatalf("BuildReport() limitations = %#v, want sorted combined limits", report.Limitations)
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	for _, privateValue := range []string{scope.OpenStackProjectID, gatewayRecord.Identity.ControllerVersion, "openStackProjectID"} {
		if strings.Contains(string(encoded), privateValue) {
			t.Fatalf("report JSON exposes private ownership value %q: %s", privateValue, encoded)
		}
	}
	for _, expectedIdentifier := range []string{"lb-1", "broken-gateway-uid", scope.ClusterID} {
		if !strings.Contains(string(encoded), expectedIdentifier) {
			t.Fatalf("report JSON does not contain documented local identifier %q: %s", expectedIdentifier, encoded)
		}
	}

	snapshot.Issues[0].Reason = "changed"
	inventory.Resources[0].Objects[0].Name = "changed"
	inventory.Limitations[0] = "changed"
	if report.KubernetesIssues[1].Reason == "changed" || report.Resources[0].Objects[0].Name == "changed" || slices.Contains(report.Limitations, "changed") {
		t.Fatal("BuildReport() retained mutable input storage")
	}
}

func TestBuildReportIsDeterministic(t *testing.T) {
	scope := reportTestScope()
	gatewayRecord, routeRecord := reportTestRecords(scope)
	issues := []KubernetesIssue{
		{Object: reportTestObject("HTTPRoute", "zeta", "route", "route-uid"), Reason: "InvalidBinding", Message: "Invalid binding"},
		{Object: reportTestObject("Gateway", "alpha", "gateway", "gateway-uid"), Reason: "InvalidBinding", Message: "Invalid binding"},
	}
	resources := []ResourceFinding{
		reportTestFinding("Octavia", "Pool", "pool-1", DispositionMatched),
		reportTestFinding("Neutron", "FloatingIP", "fip-1", DispositionUnresolved),
	}
	when := time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC)

	first, err := BuildReport(when, "test", scope,
		KubernetesSnapshot{Records: []OwnershipRecord{gatewayRecord, routeRecord}, Issues: issues},
		Inventory{Resources: resources, Limitations: []string{"B", "A"}},
	)
	if err != nil {
		t.Fatalf("first BuildReport() error = %v", err)
	}
	slices.Reverse(issues)
	slices.Reverse(resources)
	second, err := BuildReport(when, "test", scope,
		KubernetesSnapshot{Records: []OwnershipRecord{routeRecord, gatewayRecord}, Issues: issues},
		Inventory{Resources: resources, Limitations: []string{"A", "B"}},
	)
	if err != nil {
		t.Fatalf("second BuildReport() error = %v", err)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("BuildReport() is not deterministic:\n%s\n%s", firstJSON, secondJSON)
	}
}

func TestBuildReportUsesJSONArraysForEmptyResults(t *testing.T) {
	report, err := BuildReport(
		time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC),
		"test",
		reportTestScope(),
		KubernetesSnapshot{},
		Inventory{},
	)
	if err != nil {
		t.Fatalf("BuildReport() error = %v", err)
	}
	if report.Assessment != AssessmentComplete || report.HasFindings() {
		t.Fatalf("BuildReport() = %#v, want complete assessment without findings", report)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, emptyArray := range []string{`"kubernetesIssues":[]`, `"resources":[]`} {
		if !strings.Contains(string(encoded), emptyArray) {
			t.Fatalf("report JSON = %s, want %s", encoded, emptyArray)
		}
	}
}

func TestBuildReportUsesJSONArrayForEmptyFindingObjects(t *testing.T) {
	finding := reportTestFinding("Octavia", "LoadBalancer", "lb-1", DispositionOrphanCandidate)
	finding.Objects = nil
	report, err := BuildReport(
		time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC),
		"test",
		reportTestScope(),
		KubernetesSnapshot{},
		Inventory{Resources: []ResourceFinding{finding}},
	)
	if err != nil {
		t.Fatalf("BuildReport() error = %v", err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"objects":[]`) {
		t.Fatalf("report JSON = %s, want nested objects array", encoded)
	}
}

func TestBuildReportRejectsInvalidInput(t *testing.T) {
	when := time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC)
	scope := reportTestScope()
	record, _ := reportTestRecords(scope)
	mismatchedRecord := record
	mismatchedRecord.Objects = []ObjectReference{reportTestObject("Gateway", "default", "another-gateway", "another-uid")}
	validSnapshot := KubernetesSnapshot{Records: []OwnershipRecord{record}}
	validInventory := Inventory{Resources: []ResourceFinding{reportTestFinding("Octavia", "Listener", "listener-1", DispositionMatched)}}

	tests := []struct {
		name      string
		when      time.Time
		version   string
		scope     Scope
		snapshot  KubernetesSnapshot
		inventory Inventory
	}{
		{name: "zero time", version: "test", scope: scope, snapshot: validSnapshot, inventory: validInventory},
		{name: "empty version", when: when, scope: scope, snapshot: validSnapshot, inventory: validInventory},
		{name: "invalid scope", when: when, version: "test", scope: Scope{}, snapshot: validSnapshot, inventory: validInventory},
		{name: "record outside scope", when: when, version: "test", scope: scope, snapshot: KubernetesSnapshot{Records: []OwnershipRecord{{Identity: func() cloud.Identity {
			identity := record.Identity
			identity.ClusterID = "other-cluster"
			return identity
		}(), Objects: record.Objects}}}, inventory: validInventory},
		{name: "record without objects", when: when, version: "test", scope: scope, snapshot: KubernetesSnapshot{Records: []OwnershipRecord{{Identity: record.Identity}}}, inventory: validInventory},
		{name: "record object identity mismatch", when: when, version: "test", scope: scope, snapshot: KubernetesSnapshot{Records: []OwnershipRecord{mismatchedRecord}}, inventory: validInventory},
		{name: "issue without reason", when: when, version: "test", scope: scope, snapshot: KubernetesSnapshot{Issues: []KubernetesIssue{{Object: reportTestObject("Gateway", "default", "edge", "uid"), Message: "message"}}}, inventory: validInventory},
		{name: "incomplete finding", when: when, version: "test", scope: scope, snapshot: validSnapshot, inventory: Inventory{Resources: []ResourceFinding{{Disposition: DispositionMatched}}}},
		{name: "unknown disposition", when: when, version: "test", scope: scope, snapshot: validSnapshot, inventory: Inventory{Resources: []ResourceFinding{{Service: "Octavia", Type: "Listener", ID: "id", Reason: "reason", Message: "message", Disposition: "unknown"}}}},
		{name: "duplicate resource", when: when, version: "test", scope: scope, snapshot: validSnapshot, inventory: Inventory{Resources: []ResourceFinding{
			reportTestFinding("Octavia", "Listener", "listener-1", DispositionMatched),
			reportTestFinding("Octavia", "Listener", "listener-1", DispositionUnresolved),
		}}},
		{name: "empty limitation", when: when, version: "test", scope: scope, snapshot: validSnapshot, inventory: Inventory{Limitations: []string{""}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := BuildReport(test.when, test.version, test.scope, test.snapshot, test.inventory); err == nil {
				t.Fatal("BuildReport() error = nil, want invalid input")
			}
		})
	}
}

func reportTestScope() Scope {
	return Scope{
		ClusterID:          "cluster-a",
		ControllerName:     "example.com/gateway-api-openstack",
		OpenStackProjectID: "project-private",
	}
}

func reportTestRecords(scope Scope) (OwnershipRecord, OwnershipRecord) {
	gatewayIdentity := cloud.Identity{
		OpenStackProjectID: scope.OpenStackProjectID,
		ClusterID:          scope.ClusterID,
		Controller:         scope.ControllerName,
		ControllerVersion:  "private-controller-version",
		GatewayNamespace:   "default",
		GatewayName:        "edge",
		GatewayUID:         "gateway-uid",
	}
	routeIdentity := gatewayIdentity
	routeIdentity.RouteNamespace = "default"
	routeIdentity.RouteName = "app"
	routeIdentity.RouteUID = "route-uid"
	return OwnershipRecord{
			Identity: gatewayIdentity,
			Objects:  []ObjectReference{reportTestObject("Gateway", "default", "edge", "gateway-uid")},
		}, OwnershipRecord{
			Identity: routeIdentity,
			Objects:  []ObjectReference{reportTestObject("HTTPRoute", "default", "app", "route-uid")},
		}
}

func reportTestObject(kind, namespace, name, uid string) ObjectReference {
	return ObjectReference{
		APIVersion: "gateway.networking.k8s.io/v1",
		Kind:       kind,
		Namespace:  namespace,
		Name:       name,
		UID:        uid,
	}
}

func reportTestFinding(service, resourceType, id string, disposition Disposition) ResourceFinding {
	return ResourceFinding{
		Service:     service,
		Type:        resourceType,
		ID:          id,
		Disposition: disposition,
		Reason:      "TestReason",
		Message:     "Fixed test message",
		Objects:     []ObjectReference{reportTestObject("Gateway", "default", "edge", "gateway-uid")},
	}
}
