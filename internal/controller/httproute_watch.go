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

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func (r *HTTPRouteReconciler) enqueueHTTPRoutePeers(ctx context.Context, object client.Object) []reconcile.Request {
	route, ok := object.(*gatewayv1.HTTPRoute)
	if !ok {
		return nil
	}
	keys := map[types.NamespacedName]struct{}{}
	for _, gatewayKey := range parentGatewayKeys(route) {
		var routes gatewayv1.HTTPRouteList
		if err := r.List(ctx, &routes, client.MatchingFields{indexHTTPRouteByParentGateway: gatewayKey}); err != nil {
			log.FromContext(ctx).Error(err, "Could not list HTTPRoutes for parent Gateway", "gateway", gatewayKey)
			continue
		}
		for index := range routes.Items {
			keys[client.ObjectKeyFromObject(&routes.Items[index])] = struct{}{}
		}
	}
	return sortedRequests(keys)
}

func (r *HTTPRouteReconciler) enqueueHTTPRoutesForGateway(ctx context.Context, object client.Object) []reconcile.Request {
	gateway, ok := object.(*gatewayv1.Gateway)
	if !ok {
		return nil
	}
	gatewayKey := objectKeyString(client.ObjectKeyFromObject(gateway))
	keys := map[types.NamespacedName]struct{}{}
	for _, indexName := range []string{indexHTTPRouteByParentGateway, indexHTTPRouteByStatusGateway, indexHTTPRouteByBoundGateway} {
		var routes gatewayv1.HTTPRouteList
		if err := r.List(ctx, &routes, client.MatchingFields{indexName: gatewayKey}); err != nil {
			log.FromContext(ctx).Error(err, "Could not list HTTPRoutes for Gateway", "gateway", client.ObjectKeyFromObject(gateway))
			continue
		}
		for index := range routes.Items {
			keys[client.ObjectKeyFromObject(&routes.Items[index])] = struct{}{}
		}
	}
	return sortedRequests(keys)
}

func (r *HTTPRouteReconciler) enqueueHTTPRoutesForServiceKey(ctx context.Context, serviceKey types.NamespacedName) []reconcile.Request {
	var direct gatewayv1.HTTPRouteList
	if err := r.List(ctx, &direct, client.MatchingFields{
		indexHTTPRouteByBackendService: objectKeyString(serviceKey),
	}); err != nil {
		log.FromContext(ctx).Error(err, "Could not list HTTPRoutes for Service", "service", serviceKey)
		return nil
	}
	keys := map[types.NamespacedName]struct{}{}
	for index := range direct.Items {
		route := &direct.Items[index]
		keys[client.ObjectKeyFromObject(route)] = struct{}{}
		for _, gatewayKey := range parentGatewayKeys(route) {
			var peers gatewayv1.HTTPRouteList
			if err := r.List(ctx, &peers, client.MatchingFields{indexHTTPRouteByParentGateway: gatewayKey}); err != nil {
				log.FromContext(ctx).Error(err, "Could not list HTTPRoute contenders for Service", "service", serviceKey, "gateway", gatewayKey)
				continue
			}
			for peerIndex := range peers.Items {
				keys[client.ObjectKeyFromObject(&peers.Items[peerIndex])] = struct{}{}
			}
		}
	}
	return sortedRequests(keys)
}

func (r *HTTPRouteReconciler) enqueueHTTPRoutesForService(ctx context.Context, object client.Object) []reconcile.Request {
	return r.enqueueHTTPRoutesForServiceKey(ctx, client.ObjectKeyFromObject(object))
}

func (r *HTTPRouteReconciler) enqueueHTTPRoutesForEndpointSlice(ctx context.Context, object client.Object) []reconcile.Request {
	serviceName := object.GetLabels()[discoveryv1.LabelServiceName]
	if serviceName == "" {
		return nil
	}
	return r.enqueueHTTPRoutesForServiceKey(ctx, types.NamespacedName{Namespace: object.GetNamespace(), Name: serviceName})
}

func (r *HTTPRouteReconciler) enqueueHTTPRoutesForNode(ctx context.Context, object client.Object) []reconcile.Request {
	node, ok := object.(*corev1.Node)
	if !ok {
		return nil
	}
	var routes gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routes, client.MatchingFields{indexHTTPRouteByNodeBackend: nodeBackendIndexValue}); err != nil {
		log.FromContext(ctx).Error(err, "Could not list HTTPRoutes for Node", "node", node.Name)
		return nil
	}
	keys := map[types.NamespacedName]struct{}{}
	for index := range routes.Items {
		route := &routes.Items[index]
		if !structurallySupportedRoute(route) {
			continue
		}
		parents, err := r.managedParents(ctx, route)
		if err != nil {
			log.FromContext(ctx).Error(err, "Could not resolve HTTPRoute parents for Node", "httpRoute", client.ObjectKeyFromObject(route), "node", node.Name)
			continue
		}
		if len(parents) == 0 {
			continue
		}
		backend := route.Spec.Rules[0].BackendRefs[0]
		serviceKey := types.NamespacedName{Namespace: route.Namespace, Name: string(backend.Name)}
		var service corev1.Service
		if err := r.Get(ctx, serviceKey, &service); err != nil {
			if !apierrors.IsNotFound(err) {
				log.FromContext(ctx).Error(err, "Could not get backend Service for Node", "service", serviceKey, "node", node.Name)
			}
			continue
		}
		if service.Spec.Type != corev1.ServiceTypeNodePort {
			continue
		}
		if service.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal {
			local, err := r.nodeHasReadyEndpoint(ctx, serviceKey, node.Name)
			if err != nil {
				log.FromContext(ctx).Error(err, "Could not list local endpoints for Node", "service", serviceKey, "node", node.Name)
				continue
			}
			if !local {
				continue
			}
		}
		keys[client.ObjectKeyFromObject(route)] = struct{}{}
	}
	return sortedRequests(keys)
}

func (r *HTTPRouteReconciler) nodeHasReadyEndpoint(ctx context.Context, serviceKey types.NamespacedName, nodeName string) (bool, error) {
	var slices discoveryv1.EndpointSliceList
	if err := r.List(ctx, &slices, client.MatchingFields{
		indexEndpointSliceByService: objectKeyString(serviceKey),
	}); err != nil {
		return false, err
	}
	for _, slice := range slices.Items {
		for _, endpoint := range slice.Endpoints {
			if endpoint.NodeName != nil && *endpoint.NodeName == nodeName && endpointReady(endpoint.Conditions) {
				return true, nil
			}
		}
	}
	return false, nil
}

func (r *HTTPRouteReconciler) SetupWithManager(manager ctrl.Manager) error {
	if r.Coordinator == nil {
		return errGraphCoordinatorRequired
	}
	if r.APIReader == nil {
		return errAPIReaderRequired
	}
	return ctrl.NewControllerManagedBy(manager).
		For(&gatewayv1.HTTPRoute{}, builder.WithPredicates(httpRouteReconcilePredicate(r.Config))).
		// A create, spec change, or completed deletion can change the sole
		// selected route for a Phase 1 Gateway. Requeue routes for the same
		// parent Gateway and ignore status updates made by this controller.
		Watches(&gatewayv1.HTTPRoute{}, handler.EnqueueRequestsFromMapFunc(r.enqueueHTTPRoutePeers), builder.WithPredicates(httpRoutePeerPredicate())).
		Watches(&gatewayv1.Gateway{}, handler.EnqueueRequestsFromMapFunc(r.enqueueHTTPRoutesForGateway), builder.WithPredicates(gatewayForHTTPRoutePredicate(r.Config))).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(r.enqueueHTTPRoutesForService), builder.WithPredicates(servicePredicate())).
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(r.enqueueHTTPRoutesForEndpointSlice), builder.WithPredicates(endpointSlicePredicate())).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.enqueueHTTPRoutesForNode), builder.WithPredicates(nodePredicate(r.Config.NodeAddressType))).
		Named("httproute").
		Complete(r)
}
