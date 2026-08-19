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

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/audit"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

const (
	invalidBindingReason  = "InvalidBinding"
	invalidBindingMessage = "Controller binding is incomplete or does not match the configured cluster and OpenStack project"
)

// CollectOwnershipSnapshot reads the complete set of Gateway and HTTPRoute
// bindings for one controller installation. It does not mutate Kubernetes
// objects and returns no partial snapshot when either list operation fails.
func CollectOwnershipSnapshot(ctx context.Context, reader client.Reader, config Config) (audit.KubernetesSnapshot, error) {
	snapshot := audit.KubernetesSnapshot{
		Records: []audit.OwnershipRecord{},
		Issues:  []audit.KubernetesIssue{},
	}
	if reader == nil {
		return snapshot, fmt.Errorf("kubernetes API reader must not be nil")
	}
	if err := validateOwnershipSnapshotConfig(config); err != nil {
		return snapshot, err
	}

	var gateways gatewayv1.GatewayList
	if err := reader.List(ctx, &gateways); err != nil {
		return audit.KubernetesSnapshot{}, fmt.Errorf("list Gateways for ownership audit: %w", err)
	}
	var routes gatewayv1.HTTPRouteList
	if err := reader.List(ctx, &routes); err != nil {
		return audit.KubernetesSnapshot{}, fmt.Errorf("list HTTPRoutes for ownership audit: %w", err)
	}

	for index := range gateways.Items {
		gateway := &gateways.Items[index]
		if !gatewayHasControllerBinding(config, gateway) {
			continue
		}
		object := gatewayAuditReference(gateway)
		if err := validateGatewayBinding(config, gateway); err != nil {
			snapshot.Issues = append(snapshot.Issues, invalidBindingIssue(object))
			continue
		}
		identity := storedGatewayIdentity(config, gateway)
		if err := cloud.ValidateGatewayIdentity(identity); err != nil {
			snapshot.Issues = append(snapshot.Issues, invalidBindingIssue(object))
			continue
		}
		snapshot.Records = append(snapshot.Records, audit.OwnershipRecord{
			Identity: identity,
			Objects:  []audit.ObjectReference{object},
		})
	}

	for index := range routes.Items {
		route := &routes.Items[index]
		if !routeHasControllerBinding(config, route) {
			continue
		}
		object := routeAuditReference(route)
		identity, present, err := storedRouteIdentity(config, route)
		if err != nil || !present {
			snapshot.Issues = append(snapshot.Issues, invalidBindingIssue(object))
			continue
		}
		snapshot.Records = append(snapshot.Records, audit.OwnershipRecord{
			Identity: identity,
			Objects:  []audit.ObjectReference{object},
		})
	}

	sortOwnershipSnapshot(&snapshot)
	return snapshot, nil
}

func validateOwnershipSnapshotConfig(config Config) error {
	if len(config.ControllerName) > 253 || !gatewayControllerNamePattern.MatchString(string(config.ControllerName)) {
		return fmt.Errorf("controller name %q must be a domain prefixed path", config.ControllerName)
	}
	scope := audit.Scope{
		ClusterID:          config.ClusterID,
		ControllerName:     string(config.ControllerName),
		OpenStackProjectID: config.OpenStackProjectID,
	}
	if err := scope.Validate(); err != nil {
		return fmt.Errorf("validate ownership audit scope: %w", err)
	}
	return nil
}

func gatewayAuditReference(gateway *gatewayv1.Gateway) audit.ObjectReference {
	return audit.ObjectReference{
		APIVersion: gatewayv1.GroupVersion.String(),
		Kind:       "Gateway",
		Namespace:  gateway.Namespace,
		Name:       gateway.Name,
		UID:        string(gateway.UID),
	}
}

func routeAuditReference(route *gatewayv1.HTTPRoute) audit.ObjectReference {
	return audit.ObjectReference{
		APIVersion: gatewayv1.GroupVersion.String(),
		Kind:       "HTTPRoute",
		Namespace:  route.Namespace,
		Name:       route.Name,
		UID:        string(route.UID),
	}
}

func invalidBindingIssue(object audit.ObjectReference) audit.KubernetesIssue {
	return audit.KubernetesIssue{
		Object:  object,
		Reason:  invalidBindingReason,
		Message: invalidBindingMessage,
	}
}

func sortOwnershipSnapshot(snapshot *audit.KubernetesSnapshot) {
	for index := range snapshot.Records {
		snapshot.Records[index].Objects = audit.SortObjectReferences(snapshot.Records[index].Objects)
	}
	slices.SortFunc(snapshot.Records, func(left, right audit.OwnershipRecord) int {
		return strings.Compare(ownershipRecordSortKey(left), ownershipRecordSortKey(right))
	})
	slices.SortFunc(snapshot.Issues, func(left, right audit.KubernetesIssue) int {
		return strings.Compare(objectReferenceSortKey(left.Object), objectReferenceSortKey(right.Object))
	})
}

func ownershipRecordSortKey(record audit.OwnershipRecord) string {
	if len(record.Objects) == 0 {
		return ""
	}
	return objectReferenceSortKey(record.Objects[0])
}

func objectReferenceSortKey(object audit.ObjectReference) string {
	return strings.Join([]string{object.APIVersion, object.Kind, object.Namespace, object.Name, object.UID}, "\x00")
}
