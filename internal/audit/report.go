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
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

const (
	// ReportFormatVersion identifies the experimental JSON contract. Consumers
	// must reject versions they do not understand.
	ReportFormatVersion = "v1alpha1"

	// AssessmentComplete means every required list operation completed and all
	// Kubernetes bindings used for comparison were valid.
	AssessmentComplete Assessment = "complete"
	// AssessmentIncomplete means at least one Kubernetes binding could not be
	// used as ownership evidence. OpenStack orphan candidates are unsafe to act
	// on while this state remains.
	AssessmentIncomplete Assessment = "incomplete"

	readOnlyAdvisory       = "This command only reads Kubernetes and OpenStack. The report does not authorize adoption, deletion, or finalizer removal."
	consistencyAdvisory    = "Kubernetes and OpenStack are listed separately. Let reconciliation settle and run the audit again before acting on a finding."
	sharingAdvisory        = "This report contains local resource and Kubernetes object identifiers. Review and sanitize it before sharing."
	invalidBindingAdvisory = "At least one Kubernetes binding is invalid. Restore its metadata before evaluating orphan candidates."
	desiredGraphLimitation = "The audit does not verify that every Kubernetes binding has a complete desired OpenStack graph."
	gatewayAPIVersion      = "gateway.networking.k8s.io/v1"
)

// Assessment describes whether the ownership comparison had enough valid
// Kubernetes evidence. It does not claim that a complete assessment found no
// problems.
type Assessment string

// Summary contains stable counts for automation and operator review.
type Summary struct {
	KubernetesBindings int `json:"kubernetesBindings"`
	KubernetesIssues   int `json:"kubernetesIssues"`
	OpenStackResources int `json:"openStackResources"`
	Matched            int `json:"matched"`
	OrphanCandidates   int `json:"orphanCandidates"`
	StaleUIDs          int `json:"staleUIDs"`
	OwnershipConflicts int `json:"ownershipConflicts"`
	Unresolved         int `json:"unresolved"`
}

// ReportScope identifies the controller installation without retaining the
// authenticated OpenStack project ID.
type ReportScope struct {
	ClusterID      string `json:"clusterID"`
	ControllerName string `json:"controllerName"`
}

// Report is the experimental JSON produced for local operator use. It excludes
// the authenticated OpenStack project ID and private provider response data,
// but retains the resource and object identifiers needed for investigation.
type Report struct {
	FormatVersion    string            `json:"formatVersion"`
	ToolVersion      string            `json:"toolVersion"`
	GeneratedAt      time.Time         `json:"generatedAt"`
	Scope            ReportScope       `json:"scope"`
	Assessment       Assessment        `json:"assessment"`
	Summary          Summary           `json:"summary"`
	KubernetesIssues []KubernetesIssue `json:"kubernetesIssues"`
	Resources        []ResourceFinding `json:"resources"`
	Advisories       []string          `json:"advisories"`
	Limitations      []string          `json:"limitations"`
}

// BuildReport validates and copies one Kubernetes snapshot and OpenStack
// inventory into the operator report shape.
func BuildReport(generatedAt time.Time, toolVersion string, scope Scope, snapshot KubernetesSnapshot, inventory Inventory) (Report, error) {
	if generatedAt.IsZero() {
		return Report{}, fmt.Errorf("report generation time must not be zero")
	}
	if strings.TrimSpace(toolVersion) == "" {
		return Report{}, fmt.Errorf("report tool version must not be empty")
	}
	if err := scope.Validate(); err != nil {
		return Report{}, fmt.Errorf("validate report scope: %w", err)
	}
	if err := validateSnapshot(scope, snapshot); err != nil {
		return Report{}, err
	}
	if err := validateInventory(inventory); err != nil {
		return Report{}, err
	}

	report := Report{
		FormatVersion: ReportFormatVersion,
		ToolVersion:   toolVersion,
		GeneratedAt:   generatedAt.UTC(),
		Scope: ReportScope{
			ClusterID:      scope.ClusterID,
			ControllerName: scope.ControllerName,
		},
		Assessment:       AssessmentComplete,
		KubernetesIssues: copyAndSortIssues(snapshot.Issues),
		Resources:        copyAndSortFindings(inventory.Resources),
		Advisories:       []string{readOnlyAdvisory, consistencyAdvisory, sharingAdvisory},
		Limitations:      sortUniqueStrings(append(append([]string(nil), inventory.Limitations...), desiredGraphLimitation)),
	}
	if len(report.KubernetesIssues) > 0 {
		report.Assessment = AssessmentIncomplete
		report.Advisories = append(report.Advisories, invalidBindingAdvisory)
	}
	report.Summary = summarize(snapshot.Records, report.KubernetesIssues, report.Resources)
	return report, nil
}

// HasFindings reports whether the audit found invalid Kubernetes bindings or
// OpenStack resources that did not exactly match a current binding.
func (r Report) HasFindings() bool {
	return r.Summary.KubernetesIssues > 0 ||
		r.Summary.OrphanCandidates > 0 ||
		r.Summary.StaleUIDs > 0 ||
		r.Summary.OwnershipConflicts > 0 ||
		r.Summary.Unresolved > 0
}

func validateSnapshot(scope Scope, snapshot KubernetesSnapshot) error {
	for index, record := range snapshot.Records {
		if record.Identity.ClusterID != scope.ClusterID ||
			record.Identity.Controller != scope.ControllerName ||
			record.Identity.OpenStackProjectID != scope.OpenStackProjectID {
			return fmt.Errorf("ownership record %d is outside the report scope", index)
		}
		isRoute := record.Identity.RouteNamespace != "" || record.Identity.RouteName != "" || record.Identity.RouteUID != ""
		var err error
		if isRoute {
			err = cloud.ValidateRouteIdentity(record.Identity)
		} else {
			err = cloud.ValidateGatewayIdentity(record.Identity)
		}
		if err != nil {
			return fmt.Errorf("validate ownership record %d: %w", index, err)
		}
		if len(record.Objects) != 1 {
			return fmt.Errorf("ownership record %d must have exactly one Kubernetes object reference", index)
		}
		for objectIndex, object := range record.Objects {
			if err := validateObjectReference(object); err != nil {
				return fmt.Errorf("validate ownership record %d object %d: %w", index, objectIndex, err)
			}
		}
		if !objectMatchesIdentity(record.Objects[0], record.Identity, isRoute) {
			return fmt.Errorf("ownership record %d object does not match its identity", index)
		}
	}
	for index, issue := range snapshot.Issues {
		if err := validateObjectReference(issue.Object); err != nil {
			return fmt.Errorf("validate Kubernetes issue %d: %w", index, err)
		}
		if strings.TrimSpace(issue.Reason) == "" || strings.TrimSpace(issue.Message) == "" {
			return fmt.Errorf("kubernetes issue %d must have a reason and message", index)
		}
	}
	return nil
}

func validateInventory(inventory Inventory) error {
	seenResources := make(map[string]struct{}, len(inventory.Resources))
	for index, finding := range inventory.Resources {
		if strings.TrimSpace(finding.Service) == "" || strings.TrimSpace(finding.Type) == "" ||
			strings.TrimSpace(finding.ID) == "" || strings.TrimSpace(finding.Reason) == "" ||
			strings.TrimSpace(finding.Message) == "" {
			return fmt.Errorf("openstack finding %d is incomplete", index)
		}
		switch finding.Disposition {
		case DispositionMatched, DispositionOrphanCandidate, DispositionStaleUID,
			DispositionOwnershipConflict, DispositionUnresolved:
		default:
			return fmt.Errorf("openstack finding %d has an unknown disposition", index)
		}
		resourceKey := strings.Join([]string{finding.Service, finding.Type, finding.ID}, "\x00")
		if _, duplicate := seenResources[resourceKey]; duplicate {
			return fmt.Errorf("openstack finding %d duplicates a resource", index)
		}
		seenResources[resourceKey] = struct{}{}
		for objectIndex, object := range finding.Objects {
			if err := validateObjectReference(object); err != nil {
				return fmt.Errorf("validate OpenStack finding %d object %d: %w", index, objectIndex, err)
			}
		}
	}
	for index, limitation := range inventory.Limitations {
		if strings.TrimSpace(limitation) == "" {
			return fmt.Errorf("inventory limitation %d must not be empty", index)
		}
	}
	return nil
}

func objectMatchesIdentity(object ObjectReference, identity cloud.Identity, isRoute bool) bool {
	if object.APIVersion != gatewayAPIVersion {
		return false
	}
	if isRoute {
		return object.Kind == "HTTPRoute" &&
			object.Namespace == identity.RouteNamespace &&
			object.Name == identity.RouteName &&
			object.UID == identity.RouteUID
	}
	return object.Kind == "Gateway" &&
		object.Namespace == identity.GatewayNamespace &&
		object.Name == identity.GatewayName &&
		object.UID == identity.GatewayUID
}

func validateObjectReference(object ObjectReference) error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "API version", value: object.APIVersion},
		{name: "kind", value: object.Kind},
		{name: "name", value: object.Name},
		{name: "UID", value: object.UID},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s must not be empty", field.name)
		}
	}
	return nil
}

func summarize(records []OwnershipRecord, issues []KubernetesIssue, resources []ResourceFinding) Summary {
	summary := Summary{
		KubernetesBindings: len(records),
		KubernetesIssues:   len(issues),
		OpenStackResources: len(resources),
	}
	for _, finding := range resources {
		switch finding.Disposition {
		case DispositionMatched:
			summary.Matched++
		case DispositionOrphanCandidate:
			summary.OrphanCandidates++
		case DispositionStaleUID:
			summary.StaleUIDs++
		case DispositionOwnershipConflict:
			summary.OwnershipConflicts++
		case DispositionUnresolved:
			summary.Unresolved++
		}
	}
	return summary
}

func copyAndSortIssues(issues []KubernetesIssue) []KubernetesIssue {
	result := make([]KubernetesIssue, len(issues))
	copy(result, issues)
	slices.SortFunc(result, func(left, right KubernetesIssue) int {
		if compared := compareObjectReferences(left.Object, right.Object); compared != 0 {
			return compared
		}
		return strings.Compare(left.Reason+"\x00"+left.Message, right.Reason+"\x00"+right.Message)
	})
	return result
}

func copyAndSortFindings(findings []ResourceFinding) []ResourceFinding {
	result := make([]ResourceFinding, len(findings))
	copy(result, findings)
	for index := range result {
		result[index].Objects = SortObjectReferences(result[index].Objects)
		if result[index].Objects == nil {
			result[index].Objects = []ObjectReference{}
		}
	}
	slices.SortFunc(result, func(left, right ResourceFinding) int {
		leftKey := strings.Join([]string{left.Service, left.Type, left.ID, left.ParentID, left.Role, string(left.Disposition)}, "\x00")
		rightKey := strings.Join([]string{right.Service, right.Type, right.ID, right.ParentID, right.Role, string(right.Disposition)}, "\x00")
		return strings.Compare(leftKey, rightKey)
	})
	return result
}

func sortUniqueStrings(values []string) []string {
	result := append([]string(nil), values...)
	slices.Sort(result)
	return slices.Compact(result)
}
