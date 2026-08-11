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

// Package audit defines the provider-neutral ownership inventory used by the
// read-only operator audit. It deliberately contains no Kubernetes or provider
// SDK types.
package audit

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

// Scope limits one inventory to a controller installation and authenticated
// OpenStack project. The project ID is needed for safe API queries but is not
// serialized into operator reports.
type Scope struct {
	ClusterID          string `json:"clusterID"`
	ControllerName     string `json:"controllerName"`
	OpenStackProjectID string `json:"-"`
}

// Validate rejects an inventory scope that could include another controller
// installation or OpenStack project.
func (s Scope) Validate() error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "cluster ID", value: s.ClusterID},
		{name: "controller name", value: s.ControllerName},
		{name: "OpenStack project ID", value: s.OpenStackProjectID},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s must not be empty", field.name)
		}
	}
	return nil
}

// ObjectReference identifies a Kubernetes object without importing a
// Kubernetes API package into the inventory boundary.
type ObjectReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
}

// OwnershipRecord is one validated, durable Kubernetes cloud binding.
// Identity is intentionally omitted from JSON because reports should contain
// only the object references needed by an operator.
type OwnershipRecord struct {
	Identity cloud.Identity    `json:"-"`
	Objects  []ObjectReference `json:"objects"`
}

// KubernetesIssue records a binding that was present on a Kubernetes object
// but could not be used as ownership evidence. Messages are safe to include in
// an operator report and must not include annotation values.
type KubernetesIssue struct {
	Object  ObjectReference `json:"object"`
	Reason  string          `json:"reason"`
	Message string          `json:"message"`
}

// KubernetesSnapshot contains the durable ownership records observed from one
// complete Kubernetes API read. A caller must discard the snapshot if any list
// operation fails.
type KubernetesSnapshot struct {
	Records []OwnershipRecord `json:"records"`
	Issues  []KubernetesIssue `json:"issues"`
}

// Disposition describes what the inventory can prove about one resource.
// Every value is advisory because Kubernetes and OpenStack are not observed in
// a single transaction.
type Disposition string

const (
	// DispositionMatched means one current durable Kubernetes binding exactly
	// matches the resource and its observed graph relationship.
	DispositionMatched Disposition = "matched"
	// DispositionOrphanCandidate means the resource has a complete identity for
	// this controller scope but no current durable Kubernetes binding.
	DispositionOrphanCandidate Disposition = "orphanCandidate"
	// DispositionStaleUID means the stable object names match a current binding,
	// while one or more immutable Kubernetes UIDs do not.
	DispositionStaleUID Disposition = "staleUID"
	// DispositionOwnershipConflict means identity, project, cardinality, or
	// parent information is contradictory. Automated cleanup is unsafe.
	DispositionOwnershipConflict Disposition = "ownershipConflict"
	// DispositionUnresolved means related state exists but the inventory cannot
	// prove whether it is current or orphaned.
	DispositionUnresolved Disposition = "unresolved"
)

// ResourceFinding is the redacted inventory result for one OpenStack resource.
// It never contains addresses, raw tags, descriptions, API response bodies, or
// authentication data.
type ResourceFinding struct {
	Service            string            `json:"service"`
	Type               string            `json:"type"`
	ID                 string            `json:"id"`
	ParentID           string            `json:"parentID,omitempty"`
	Role               string            `json:"role,omitempty"`
	ProvisioningStatus string            `json:"provisioningStatus,omitempty"`
	Disposition        Disposition       `json:"disposition"`
	Reason             string            `json:"reason"`
	Message            string            `json:"message"`
	Objects            []ObjectReference `json:"objects"`
}

// Inventory contains one complete OpenStack scan and its known limitations.
type Inventory struct {
	Resources   []ResourceFinding `json:"resources"`
	Limitations []string          `json:"limitations"`
}

// Scanner observes resources without adopting, mutating, or deleting them.
type Scanner interface {
	Scan(context.Context, Scope, []OwnershipRecord) (Inventory, error)
}

// SortObjectReferences returns a sorted, duplicate-free copy.
func SortObjectReferences(objects []ObjectReference) []ObjectReference {
	result := append([]ObjectReference(nil), objects...)
	slices.SortFunc(result, compareObjectReferences)
	return slices.CompactFunc(result, func(left, right ObjectReference) bool {
		return compareObjectReferences(left, right) == 0
	})
}

func compareObjectReferences(left, right ObjectReference) int {
	leftKey := strings.Join([]string{left.APIVersion, left.Kind, left.Namespace, left.Name, left.UID}, "\x00")
	rightKey := strings.Join([]string{right.APIVersion, right.Kind, right.Namespace, right.Name, right.UID}, "\x00")
	return strings.Compare(leftKey, rightKey)
}
