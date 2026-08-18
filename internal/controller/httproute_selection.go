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
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func (r *HTTPRouteReconciler) isSelectedRoute(ctx context.Context, route *gatewayv1.HTTPRoute, parent managedRouteParent) (bool, error) {
	return r.isSelectedRouteWithReader(ctx, r.Client, route, parent, true)
}

func (r *HTTPRouteReconciler) isSelectedRouteWithReader(
	ctx context.Context,
	reader client.Reader,
	route *gatewayv1.HTTPRoute,
	parent managedRouteParent,
	useCacheIndex bool,
) (bool, error) {
	var routes gatewayv1.HTTPRouteList
	listOptions := []client.ListOption{client.InNamespace(route.Namespace)}
	if useCacheIndex {
		listOptions = append(listOptions, client.MatchingFields{
			indexHTTPRouteByParentGateway: objectKeyString(client.ObjectKeyFromObject(parent.gateway)),
		})
	}
	if err := reader.List(ctx, &routes, listOptions...); err != nil {
		return false, fmt.Errorf("list HTTPRoutes for Phase 1 attachment selection: %w", err)
	}
	winner := route
	for index := range routes.Items {
		candidate := &routes.Items[index]
		if candidate.Name == route.Name || !candidate.DeletionTimestamp.IsZero() || len(candidate.Spec.ParentRefs) != 1 {
			continue
		}
		candidateParent := managedRouteParent{ref: candidate.Spec.ParentRefs[0], gateway: parent.gateway, listener: parent.listener}
		if !sameGatewayTarget(candidateParent.ref, candidate.Namespace, parent.ref, route.Namespace) ||
			validateRouteParent(candidate, candidateParent) != nil || !structurallySupportedRoute(candidate) {
			continue
		}
		canReserve, err := r.routeCanReserveSlotWithReader(ctx, reader, candidate)
		if err != nil {
			return false, err
		}
		if !canReserve {
			continue
		}
		if routePrecedes(candidate, winner) {
			winner = candidate
		}
	}
	return winner.Name == route.Name, nil
}

type routeSlotDecision struct {
	canReserve bool
	rejection  error
}

// evaluateRouteSlot excludes a definitively rejected NodePort profile while
// allowing an otherwise valid route with temporarily unresolved references to
// retain deterministic selection. Callers must first verify that the route is
// structurally supported.
func (r *HTTPRouteReconciler) evaluateRouteSlot(ctx context.Context, route *gatewayv1.HTTPRoute) (routeSlotDecision, error) {
	return r.evaluateRouteSlotWithReader(ctx, r.Client, route)
}

func (r *HTTPRouteReconciler) evaluateRouteSlotWithReader(
	ctx context.Context,
	reader client.Reader,
	route *gatewayv1.HTTPRoute,
) (routeSlotDecision, error) {
	backend := route.Spec.Rules[0].BackendRefs[0]
	var service corev1.Service
	if err := reader.Get(ctx, types.NamespacedName{Namespace: route.Namespace, Name: string(backend.Name)}, &service); err != nil {
		if apierrors.IsNotFound(err) {
			return routeSlotDecision{canReserve: true}, nil
		}
		return routeSlotDecision{}, fmt.Errorf("get candidate backend Service: %w", err)
	}
	if service.Spec.Type != corev1.ServiceTypeNodePort {
		return routeSlotDecision{rejection: newRouteBuildError(routeErrorUnsupported, "backend Service %s must be type NodePort in Phase 1", service.Name)}, nil
	}
	if _, err := nodePortForBackend(&service, int32(*backend.Port)); err != nil {
		var buildError *routeBuildError
		if errors.As(err, &buildError) && buildError.kind == routeErrorPending {
			return routeSlotDecision{canReserve: true}, nil
		}
		return routeSlotDecision{rejection: err}, nil
	}
	return routeSlotDecision{canReserve: true}, nil
}

func (r *HTTPRouteReconciler) routeCanReserveSlot(ctx context.Context, route *gatewayv1.HTTPRoute) (bool, error) {
	return r.routeCanReserveSlotWithReader(ctx, r.Client, route)
}

func (r *HTTPRouteReconciler) routeCanReserveSlotWithReader(
	ctx context.Context,
	reader client.Reader,
	route *gatewayv1.HTTPRoute,
) (bool, error) {
	decision, err := r.evaluateRouteSlotWithReader(ctx, reader, route)
	return decision.canReserve, err
}

func routePrecedes(left, right *gatewayv1.HTTPRoute) bool {
	if !left.CreationTimestamp.Equal(&right.CreationTimestamp) {
		return left.CreationTimestamp.Before(&right.CreationTimestamp)
	}
	return left.Namespace+"/"+left.Name < right.Namespace+"/"+right.Name
}
