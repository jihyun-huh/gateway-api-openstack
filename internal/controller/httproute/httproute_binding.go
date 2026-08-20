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

package httproute

import (
	"context"
	"reflect"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
)

func (r *Reconciler) bindRoute(ctx context.Context, route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) (bool, error) {
	if r.APIReader == nil {
		return false, controller.ErrAPIReaderRequired
	}
	snapshot, err := r.buildRouteValidationSnapshot(ctx, route, gateway, routeSnapshotForBinding)
	if err != nil {
		return false, err
	}
	return r.bindRouteFromSnapshot(ctx, route, gateway, snapshot)
}

// bindRouteFromSnapshot uses the model already built while holding the
// Gateway graph lock. A conflict retry rebuilds the snapshot because the
// original observation is no longer current.
func (r *Reconciler) bindRouteFromSnapshot(
	ctx context.Context,
	route *gatewayv1.HTTPRoute,
	gateway *gatewayv1.Gateway,
	initial routeValidationSnapshot,
) (bool, error) {
	bindingStored := false
	snapshot := initial
	firstAttempt := true
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if firstAttempt {
			firstAttempt = false
		} else {
			refreshed, err := r.buildRouteValidationSnapshot(ctx, route, gateway, routeSnapshotForBinding)
			if err != nil {
				return err
			}
			snapshot = refreshed
		}
		if err := r.validateRouteBindingTransition(snapshot); err != nil {
			return err
		}
		if err := r.sealRouteMutationSnapshot(ctx, snapshot, routeBindingOptional); err != nil {
			return err
		}

		base := snapshot.route.DeepCopy()
		desired := snapshot.route.DeepCopy()
		r.applyRouteBinding(desired, snapshot.gateway)
		if routeBindingMetadataEqual(base, desired) {
			*route = *desired
			return nil
		}
		if err := r.Patch(ctx, desired, controller.OptimisticMergeFrom(base)); err != nil {
			return err
		}
		bindingStored = true
		*route = *desired
		log.FromContext(ctx).V(1).Info("Bound HTTPRoute to Gateway", "gateway", client.ObjectKeyFromObject(snapshot.gateway))
		return nil
	})
	return bindingStored, err
}

func (r *Reconciler) getRouteBinding(
	ctx context.Context,
	routeKey types.NamespacedName,
	routeUID types.UID,
	generation int64,
	isDeleting bool,
) (*gatewayv1.HTTPRoute, cloud.Identity, bool, error) {
	return r.getRouteBindingWithReader(ctx, r.Client, routeKey, routeUID, generation, isDeleting)
}

func (r *Reconciler) getRouteBindingWithReader(
	ctx context.Context,
	reader client.Reader,
	routeKey types.NamespacedName,
	routeUID types.UID,
	generation int64,
	isDeleting bool,
) (*gatewayv1.HTTPRoute, cloud.Identity, bool, error) {
	current := &gatewayv1.HTTPRoute{}
	if err := reader.Get(ctx, routeKey, current); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, cloud.Identity{}, false, nil
		}
		return nil, cloud.Identity{}, false, err
	}
	if !sameHTTPRouteRevision(current, routeUID, generation, isDeleting) {
		return current, cloud.Identity{}, false, errHTTPRouteChanged
	}
	stored, present, err := r.storedRouteIdentity(current)
	return current, stored, present, err
}

func (r *Reconciler) clearStoredRouteBinding(
	ctx context.Context,
	route *gatewayv1.HTTPRoute,
	expected cloud.Identity,
	expectedPresent bool,
) error {
	if r.APIReader == nil {
		return controller.ErrAPIReaderRequired
	}
	routeKey := client.ObjectKeyFromObject(route)
	routeUID, generation, isDeleting := route.UID, route.Generation, !route.DeletionTimestamp.IsZero()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, stored, present, err := r.getRouteBindingWithReader(
			ctx,
			r.APIReader,
			routeKey,
			routeUID,
			generation,
			isDeleting,
		)
		if err != nil {
			return err
		}
		if current == nil {
			return nil
		}
		if present != expectedPresent || present && stored != expected {
			return errHTTPRouteChanged
		}
		base := current.DeepCopy()
		r.clearRouteBinding(current)
		if routeBindingMetadataEqual(base, current) {
			*route = *current
			return nil
		}
		if err := r.Patch(ctx, current, controller.OptimisticMergeFrom(base)); err != nil {
			return client.IgnoreNotFound(err)
		}
		*route = *current
		return nil
	})
}

func (r *Reconciler) applyRouteBinding(route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) {
	if route.Annotations == nil {
		route.Annotations = map[string]string{}
	}
	route.Annotations[r.Config.RouteGatewayNamespaceAnnotation()] = gateway.Namespace
	route.Annotations[r.Config.RouteGatewayNameAnnotation()] = gateway.Name
	route.Annotations[r.Config.RouteGatewayUIDAnnotation()] = string(gateway.UID)
	route.Annotations[r.Config.RouteClusterIDAnnotation()] = r.Config.ClusterID
	route.Annotations[r.Config.RouteProjectIDAnnotation()] = r.Config.OpenStackProjectID
	delete(route.Annotations, r.Config.RouteCleanupFailureAnnotation())
	bindingFinalizer := r.Config.RouteBindingFinalizer(
		r.Config.ClusterID,
		r.Config.OpenStackProjectID,
		gateway.Namespace,
		gateway.Name,
		string(gateway.UID),
	)
	r.removeStaleRouteBindingFinalizers(route, bindingFinalizer)
	controllerutil.AddFinalizer(route, bindingFinalizer)
	controllerutil.AddFinalizer(route, r.Config.RouteFinalizer())
}

func (r *Reconciler) clearRouteBinding(route *gatewayv1.HTTPRoute) {
	controllerutil.RemoveFinalizer(route, r.Config.RouteFinalizer())
	r.removeRouteBindingFinalizers(route)
	delete(route.Annotations, r.Config.RouteGatewayNamespaceAnnotation())
	delete(route.Annotations, r.Config.RouteGatewayNameAnnotation())
	delete(route.Annotations, r.Config.RouteGatewayUIDAnnotation())
	delete(route.Annotations, r.Config.RouteClusterIDAnnotation())
	delete(route.Annotations, r.Config.RouteProjectIDAnnotation())
	delete(route.Annotations, r.Config.RouteCleanupFailureAnnotation())
}

func (r *Reconciler) removeStaleRouteBindingFinalizers(route *gatewayv1.HTTPRoute, expected string) {
	finalizers := make([]string, 0, len(route.Finalizers))
	for _, finalizer := range route.Finalizers {
		if strings.HasPrefix(finalizer, r.Config.RouteBindingFinalizerPrefix()) && finalizer != expected {
			continue
		}
		finalizers = append(finalizers, finalizer)
	}
	route.Finalizers = finalizers
}

func routeBindingMetadataEqual(left, right *gatewayv1.HTTPRoute) bool {
	return reflect.DeepEqual(left.Annotations, right.Annotations) && reflect.DeepEqual(left.Finalizers, right.Finalizers)
}

func sameHTTPRouteRevision(route *gatewayv1.HTTPRoute, uid types.UID, generation int64, isDeleting bool) bool {
	currentIsDeleting := !route.DeletionTimestamp.IsZero()
	return route.UID == uid && route.Generation == generation && currentIsDeleting == isDeleting
}

func (r *Reconciler) storedRouteIdentity(route *gatewayv1.HTTPRoute) (cloud.Identity, bool, error) {
	return controller.StoredRouteIdentity(r.Config, route)
}

func (r *Reconciler) removeRouteBindingFinalizers(route *gatewayv1.HTTPRoute) {
	for _, finalizer := range append([]string(nil), route.Finalizers...) {
		if strings.HasPrefix(finalizer, r.Config.RouteBindingFinalizerPrefix()) {
			controllerutil.RemoveFinalizer(route, finalizer)
		}
	}
}
