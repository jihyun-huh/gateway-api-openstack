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

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func (r *GatewayReconciler) gatewayRequestsForHTTPRoute(_ context.Context, object client.Object) []reconcile.Request {
	route, ok := object.(*gatewayv1.HTTPRoute)
	if !ok {
		return nil
	}

	keys := make(map[types.NamespacedName]struct{}, len(route.Spec.ParentRefs)+len(route.Status.Parents)+1)
	addParent := func(parentRef gatewayv1.ParentReference) {
		if !isGatewayParentRef(parentRef) || parentRef.Name == "" {
			return
		}
		gatewayNamespace := route.Namespace
		if parentRef.Namespace != nil {
			gatewayNamespace = string(*parentRef.Namespace)
		}
		keys[types.NamespacedName{Namespace: gatewayNamespace, Name: string(parentRef.Name)}] = struct{}{}
	}
	for _, parentRef := range route.Spec.ParentRefs {
		addParent(parentRef)
	}
	for _, parentStatus := range route.Status.Parents {
		if parentStatus.ControllerName != r.Config.ControllerName {
			continue
		}
		addParent(parentStatus.ParentRef)
	}
	boundNamespace := route.Annotations[r.Config.routeGatewayNamespaceAnnotation()]
	boundName := route.Annotations[r.Config.routeGatewayNameAnnotation()]
	boundUID := route.Annotations[r.Config.routeGatewayUIDAnnotation()]
	if boundNamespace != "" && boundName != "" && boundUID != "" {
		keys[types.NamespacedName{Namespace: boundNamespace, Name: boundName}] = struct{}{}
	}
	return sortedRequests(keys)
}

func (r *GatewayReconciler) gatewayRequestsForGatewayClass(ctx context.Context, object client.Object) []reconcile.Request {
	gatewayClass, ok := object.(*gatewayv1.GatewayClass)
	if !ok {
		return nil
	}

	var gateways gatewayv1.GatewayList
	if err := r.List(ctx, &gateways, client.MatchingFields{
		indexGatewayByClass: gatewayClass.Name,
	}); err != nil {
		log.FromContext(ctx).Error(err, "Could not list Gateways for GatewayClass", "gatewayClass", gatewayClass.Name)
		return nil
	}
	keys := make(map[types.NamespacedName]struct{}, len(gateways.Items))
	for _, gateway := range gateways.Items {
		keys[client.ObjectKeyFromObject(&gateway)] = struct{}{}
	}
	return sortedRequests(keys)
}

func (r *GatewayReconciler) SetupWithManager(manager ctrl.Manager) error {
	if r.Coordinator == nil {
		return errGraphCoordinatorRequired
	}
	if r.APIReader == nil {
		return errAPIReaderRequired
	}
	return ctrl.NewControllerManagedBy(manager).
		For(&gatewayv1.Gateway{}, builder.WithPredicates(gatewayReconcilePredicate(r.Config))).
		Watches(
			&gatewayv1.HTTPRoute{},
			handler.EnqueueRequestsFromMapFunc(r.gatewayRequestsForHTTPRoute),
			builder.WithPredicates(httpRouteForGatewayPredicate(r.Config)),
		).
		Watches(
			&gatewayv1.GatewayClass{},
			handler.EnqueueRequestsFromMapFunc(r.gatewayRequestsForGatewayClass),
			builder.WithPredicates(gatewayClassForGatewayPredicate()),
		).
		Named("gateway").
		Complete(r)
}
