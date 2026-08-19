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
	"net"
	"sort"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

func nodePortForBackend(service *corev1.Service, port int32) (*corev1.ServicePort, error) {
	for index := range service.Spec.Ports {
		candidate := &service.Spec.Ports[index]
		if candidate.Port != port {
			continue
		}
		if candidate.Protocol != "" && candidate.Protocol != corev1.ProtocolTCP {
			return nil, newRouteBuildError(routeErrorUnsupportedProtocol, "backend Service port %d must use TCP", port)
		}
		if candidate.AppProtocol != nil && *candidate.AppProtocol != "http" && *candidate.AppProtocol != "kubernetes.io/http" {
			return nil, newRouteBuildError(routeErrorUnsupportedProtocol, "backend Service appProtocol %q is unsupported", *candidate.AppProtocol)
		}
		if candidate.NodePort == 0 {
			return nil, newRouteBuildError(routeErrorUnsupported, "backend Service port %d has no allocated NodePort", port)
		}
		return candidate, nil
	}
	return nil, newRouteBuildError(routeErrorPending, "backend Service %s does not expose port %d", service.Name, port)
}

func (r *HTTPRouteReconciler) nodePortMembers(ctx context.Context, service *corev1.Service, nodePort int32) ([]cloud.Member, error) {
	return r.nodePortMembersWithReader(ctx, r.Client, service, nodePort)
}

func (r *HTTPRouteReconciler) nodePortMembersWithReader(
	ctx context.Context,
	reader client.Reader,
	service *corev1.Service,
	nodePort int32,
) ([]cloud.Member, error) {
	var slices discoveryv1.EndpointSliceList
	if err := reader.List(
		ctx,
		&slices,
		client.InNamespace(service.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: service.Name},
	); err != nil {
		return nil, fmt.Errorf("list backend EndpointSlices: %w", err)
	}
	readyEndpoint := false
	localNodes := map[string]struct{}{}
	for _, slice := range slices.Items {
		for _, endpoint := range slice.Endpoints {
			if !endpointReady(endpoint.Conditions) {
				continue
			}
			readyEndpoint = true
			if endpoint.NodeName != nil {
				localNodes[*endpoint.NodeName] = struct{}{}
			}
		}
	}
	if !readyEndpoint {
		return nil, newRouteBuildError(routeErrorPending, "backend Service %s has no ready endpoints", service.Name)
	}
	if service.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal && len(localNodes) == 0 {
		return nil, newRouteBuildError(routeErrorPending, "backend Service %s uses externalTrafficPolicy Local but ready endpoints do not identify their Nodes", service.Name)
	}

	var nodes corev1.NodeList
	if err := reader.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("list backend Nodes: %w", err)
	}
	members := make([]cloud.Member, 0, len(nodes.Items))
	seen := map[string]struct{}{}
	for index := range nodes.Items {
		node := &nodes.Items[index]
		if !readyNode(node) {
			continue
		}
		if service.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal {
			if _, ok := localNodes[node.Name]; !ok {
				continue
			}
		}
		address := nodeAddress(node, r.Config.NodeAddressType)
		if address == "" {
			continue
		}
		key := net.JoinHostPort(address, fmt.Sprintf("%d", nodePort))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		members = append(members, cloud.Member{Address: address, Port: int(nodePort)})
	}
	if len(members) == 0 {
		return nil, newRouteBuildError(routeErrorPending, "backend Service %s has no ready Nodes with a %s address", service.Name, r.Config.NodeAddressType)
	}
	sort.Slice(members, func(i, j int) bool {
		if members[i].Address == members[j].Address {
			return members[i].Port < members[j].Port
		}
		return members[i].Address < members[j].Address
	})
	return members, nil
}

func endpointReady(conditions discoveryv1.EndpointConditions) bool {
	if conditions.Terminating != nil && *conditions.Terminating {
		return false
	}
	return conditions.Ready == nil || *conditions.Ready
}

func readyNode(node *corev1.Node) bool {
	if !node.DeletionTimestamp.IsZero() || node.Spec.Unschedulable {
		return false
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func nodeAddress(node *corev1.Node, addressType corev1.NodeAddressType) string {
	for _, address := range node.Status.Addresses {
		if address.Type == addressType && net.ParseIP(address.Address) != nil {
			return address.Address
		}
	}
	return ""
}
