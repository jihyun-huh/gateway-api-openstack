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
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
	gatewaycontroller "github.com/jihyun-huh/gateway-api-openstack/internal/controller/gateway"
)

func (r *Reconciler) bindRoute(ctx context.Context, route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) (bool, error) {
	if r.APIReader == nil {
		return false, controller.ErrAPIReaderRequired
	}
	routeKey := client.ObjectKeyFromObject(route)
	routeUID, generation, isDeleting := route.UID, route.Generation, !route.DeletionTimestamp.IsZero()
	bindingStored := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &gatewayv1.HTTPRoute{}
		if err := r.APIReader.Get(ctx, routeKey, current); err != nil {
			return errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
		}
		if !sameHTTPRouteRevision(current, routeUID, generation, isDeleting) {
			return errHTTPRouteChanged
		}
		var liveGateway gatewayv1.Gateway
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(gateway), &liveGateway); err != nil {
			return errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
		}
		if liveGateway.UID != gateway.UID || liveGateway.Generation != gateway.Generation ||
			!liveGateway.DeletionTimestamp.IsZero() {
			return errHTTPRouteChanged
		}
		var gatewayClass gatewayv1.GatewayClass
		if err := r.APIReader.Get(
			ctx,
			types.NamespacedName{Name: string(liveGateway.Spec.GatewayClassName)},
			&gatewayClass,
		); err != nil {
			return errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
		}
		if gatewayClass.Spec.ControllerName != r.Config.ControllerName || gatewayClass.Spec.ParametersRef != nil {
			return errHTTPRouteChanged
		}
		if !controller.GatewayClassSupportsInstalledVersion(&gatewayClass) {
			return controller.ErrUnsupportedGatewayAPIVersion
		}
		if !controllerutil.ContainsFinalizer(&liveGateway, r.Config.GatewayFinalizer()) ||
			controller.ValidateGatewayBinding(r.Config, &liveGateway) != nil {
			return errHTTPRouteChanged
		}
		listener, validationErr := gatewaycontroller.Validate(&liveGateway)
		if validationErr != nil || len(current.Spec.ParentRefs) != 1 {
			return errHTTPRouteChanged
		}
		parent := managedRouteParent{
			ref: current.Spec.ParentRefs[0], gateway: &liveGateway,
			gatewayClass: &gatewayClass, listener: listener,
		}
		if validateRouteParent(current, parent) != nil ||
			liveGateway.Annotations[r.Config.GatewayListenerPortAnnotation()] != strconv.Itoa(int(listener.Port)) {
			return errHTTPRouteChanged
		}
		programmed := meta.FindStatusCondition(
			liveGateway.Status.Conditions,
			string(gatewayv1.GatewayConditionProgrammed),
		)
		if programmed == nil || programmed.Status != metav1.ConditionTrue ||
			programmed.ObservedGeneration != liveGateway.Generation {
			return errHTTPRouteChanged
		}
		if !structurallySupportedRoute(current) {
			return errHTTPRouteChanged
		}
		decision, err := r.evaluateRouteSlotWithReader(ctx, r.APIReader, current)
		if err != nil || !decision.canReserve {
			return errors.Join(errHTTPRouteChanged, err)
		}
		selected, err := r.isSelectedRouteWithReader(ctx, r.APIReader, current, parent, false)
		if err != nil || !selected {
			return errors.Join(errHTTPRouteChanged, err)
		}
		if _, err := r.buildRouteSpecWithReader(ctx, r.APIReader, current, &liveGateway); err != nil {
			return errors.Join(errHTTPRouteChanged, err)
		}
		desired := controller.RouteIdentity(r.Config, &liveGateway, current)

		stored, present, err := r.storedRouteIdentity(current)
		if err != nil {
			return err
		}
		if !present && controllerutil.ContainsFinalizer(current, r.Config.RouteFinalizer()) {
			return fmt.Errorf("HTTPRoute %s has the controller finalizer but no complete stored Gateway identity", routeKey)
		}
		if present && !controller.SameGatewayIdentity(stored, desired) {
			return errHTTPRouteChanged
		}
		if err := r.validateRouteMutationOwnership(ctx, current, &liveGateway, false); err != nil {
			return err
		}

		base := current.DeepCopy()
		r.applyRouteBinding(current, &liveGateway)
		if routeBindingMetadataEqual(base, current) {
			*route = *current
			return nil
		}
		if err := r.Patch(ctx, current, controller.OptimisticMergeFrom(base)); err != nil {
			return err
		}
		bindingStored = true
		*route = *current
		log.FromContext(ctx).V(1).Info("Bound HTTPRoute to Gateway", "gateway", client.ObjectKeyFromObject(&liveGateway))
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
