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
)

func (r *HTTPRouteReconciler) bindRoute(ctx context.Context, route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) (bool, error) {
	if r.APIReader == nil {
		return false, errAPIReaderRequired
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
		if !gatewayClassSupportsInstalledVersion(&gatewayClass) {
			return errUnsupportedGatewayAPIVersion
		}
		if !controllerutil.ContainsFinalizer(&liveGateway, r.Config.gatewayFinalizer()) ||
			validateGatewayBinding(r.Config, &liveGateway) != nil {
			return errHTTPRouteChanged
		}
		listener, validationErr := validateGateway(&liveGateway)
		if validationErr != nil || len(current.Spec.ParentRefs) != 1 {
			return errHTTPRouteChanged
		}
		parent := managedRouteParent{
			ref: current.Spec.ParentRefs[0], gateway: &liveGateway,
			gatewayClass: &gatewayClass, listener: listener,
		}
		if validateRouteParent(current, parent) != nil ||
			liveGateway.Annotations[r.Config.gatewayListenerPortAnnotation()] != strconv.Itoa(int(listener.Port)) {
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
		desired := routeIdentity(r.Config, &liveGateway, current)

		stored, present, err := r.storedRouteIdentity(current)
		if err != nil {
			return err
		}
		if !present && controllerutil.ContainsFinalizer(current, r.Config.routeFinalizer()) {
			return fmt.Errorf("HTTPRoute %s has the controller finalizer but no complete stored Gateway identity", routeKey)
		}
		if present && !sameGatewayIdentity(stored, desired) {
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
		if err := r.Patch(ctx, current, optimisticMergeFrom(base)); err != nil {
			return err
		}
		bindingStored = true
		*route = *current
		log.FromContext(ctx).V(1).Info("Bound HTTPRoute to Gateway", "gateway", client.ObjectKeyFromObject(&liveGateway))
		return nil
	})
	return bindingStored, err
}

func (r *HTTPRouteReconciler) getRouteBinding(
	ctx context.Context,
	routeKey types.NamespacedName,
	routeUID types.UID,
	generation int64,
	isDeleting bool,
) (*gatewayv1.HTTPRoute, cloud.Identity, bool, error) {
	return r.getRouteBindingWithReader(ctx, r.Client, routeKey, routeUID, generation, isDeleting)
}

func (r *HTTPRouteReconciler) getRouteBindingWithReader(
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

func (r *HTTPRouteReconciler) clearStoredRouteBinding(
	ctx context.Context,
	route *gatewayv1.HTTPRoute,
	expected cloud.Identity,
	expectedPresent bool,
) error {
	if r.APIReader == nil {
		return errAPIReaderRequired
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
		if err := r.Patch(ctx, current, optimisticMergeFrom(base)); err != nil {
			return client.IgnoreNotFound(err)
		}
		*route = *current
		return nil
	})
}

func sameGatewayIdentity(left, right cloud.Identity) bool {
	return left.GatewayNamespace == right.GatewayNamespace &&
		left.GatewayName == right.GatewayName &&
		left.GatewayUID == right.GatewayUID
}

func (r *HTTPRouteReconciler) applyRouteBinding(route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) {
	if route.Annotations == nil {
		route.Annotations = map[string]string{}
	}
	route.Annotations[r.Config.routeGatewayNamespaceAnnotation()] = gateway.Namespace
	route.Annotations[r.Config.routeGatewayNameAnnotation()] = gateway.Name
	route.Annotations[r.Config.routeGatewayUIDAnnotation()] = string(gateway.UID)
	route.Annotations[r.Config.routeClusterIDAnnotation()] = r.Config.ClusterID
	route.Annotations[r.Config.routeProjectIDAnnotation()] = r.Config.OpenStackProjectID
	delete(route.Annotations, r.Config.routeCleanupFailureAnnotation())
	bindingFinalizer := r.Config.routeBindingFinalizer(
		r.Config.ClusterID,
		r.Config.OpenStackProjectID,
		gateway.Namespace,
		gateway.Name,
		string(gateway.UID),
	)
	r.removeStaleRouteBindingFinalizers(route, bindingFinalizer)
	controllerutil.AddFinalizer(route, bindingFinalizer)
	controllerutil.AddFinalizer(route, r.Config.routeFinalizer())
}

func (r *HTTPRouteReconciler) clearRouteBinding(route *gatewayv1.HTTPRoute) {
	controllerutil.RemoveFinalizer(route, r.Config.routeFinalizer())
	r.removeRouteBindingFinalizers(route)
	delete(route.Annotations, r.Config.routeGatewayNamespaceAnnotation())
	delete(route.Annotations, r.Config.routeGatewayNameAnnotation())
	delete(route.Annotations, r.Config.routeGatewayUIDAnnotation())
	delete(route.Annotations, r.Config.routeClusterIDAnnotation())
	delete(route.Annotations, r.Config.routeProjectIDAnnotation())
	delete(route.Annotations, r.Config.routeCleanupFailureAnnotation())
}

func (r *HTTPRouteReconciler) removeStaleRouteBindingFinalizers(route *gatewayv1.HTTPRoute, expected string) {
	finalizers := make([]string, 0, len(route.Finalizers))
	for _, finalizer := range route.Finalizers {
		if strings.HasPrefix(finalizer, r.Config.routeBindingFinalizerPrefix()) && finalizer != expected {
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

func (r *HTTPRouteReconciler) storedRouteIdentity(route *gatewayv1.HTTPRoute) (cloud.Identity, bool, error) {
	return storedRouteIdentity(r.Config, route)
}

func storedRouteIdentity(config Config, route *gatewayv1.HTTPRoute) (cloud.Identity, bool, error) {
	annotations := route.Annotations
	values := []string{
		annotations[config.routeGatewayNamespaceAnnotation()],
		annotations[config.routeGatewayNameAnnotation()],
		annotations[config.routeGatewayUIDAnnotation()],
	}
	empty := 0
	for _, value := range values {
		if value == "" {
			empty++
		}
	}
	if empty == len(values) {
		if annotations[config.routeClusterIDAnnotation()] != "" || annotations[config.routeProjectIDAnnotation()] != "" || hasRouteBindingFinalizer(config, route) {
			return cloud.Identity{}, false, fmt.Errorf("HTTPRoute %s/%s has an incomplete stored Gateway identity", route.Namespace, route.Name)
		}
		return cloud.Identity{}, false, nil
	}
	if empty != 0 {
		return cloud.Identity{}, false, fmt.Errorf("HTTPRoute %s/%s has an incomplete stored Gateway identity", route.Namespace, route.Name)
	}
	clusterID := annotations[config.routeClusterIDAnnotation()]
	if clusterID == "" {
		clusterID = config.ClusterID
	}
	projectID := annotations[config.routeProjectIDAnnotation()]
	if projectID == "" {
		projectID = config.OpenStackProjectID
	}
	identity := cloud.Identity{
		OpenStackProjectID: projectID,
		ClusterID:          clusterID,
		Controller:         string(config.ControllerName),
		ControllerVersion:  config.ControllerVersion,
		GatewayNamespace:   values[0],
		GatewayName:        values[1],
		GatewayUID:         values[2],
		RouteNamespace:     route.Namespace,
		RouteName:          route.Name,
		RouteUID:           string(route.UID),
	}
	if identity.ClusterID != config.ClusterID {
		return cloud.Identity{}, false, fmt.Errorf("controller cluster identity differs from HTTPRoute %s/%s's existing resource binding; restore the original cluster ID before reconciling", route.Namespace, route.Name)
	}
	if identity.OpenStackProjectID != config.OpenStackProjectID {
		return cloud.Identity{}, false, fmt.Errorf("authenticated OpenStack project differs from HTTPRoute %s/%s's existing resource binding; restore access to the original project before reconciling", route.Namespace, route.Name)
	}
	if err := cloud.ValidateRouteIdentity(identity); err != nil {
		return cloud.Identity{}, false, err
	}
	expectedBindingFinalizer := config.routeBindingFinalizer(
		identity.ClusterID,
		identity.OpenStackProjectID,
		identity.GatewayNamespace,
		identity.GatewayName,
		identity.GatewayUID,
	)
	if controllerutil.ContainsFinalizer(route, config.routeFinalizer()) && !controllerutil.ContainsFinalizer(route, expectedBindingFinalizer) {
		return cloud.Identity{}, false, fmt.Errorf("HTTPRoute %s/%s stored Gateway identity does not match its binding finalizer; restore the original binding before reconciling", route.Namespace, route.Name)
	}
	for _, finalizer := range route.Finalizers {
		if strings.HasPrefix(finalizer, config.routeBindingFinalizerPrefix()) && finalizer != expectedBindingFinalizer {
			return cloud.Identity{}, false, fmt.Errorf("HTTPRoute %s/%s has a conflicting Gateway binding finalizer", route.Namespace, route.Name)
		}
	}
	return identity, true, nil
}

func (r *HTTPRouteReconciler) removeRouteBindingFinalizers(route *gatewayv1.HTTPRoute) {
	for _, finalizer := range append([]string(nil), route.Finalizers...) {
		if strings.HasPrefix(finalizer, r.Config.routeBindingFinalizerPrefix()) {
			controllerutil.RemoveFinalizer(route, finalizer)
		}
	}
}

func hasRouteBindingFinalizer(config Config, route *gatewayv1.HTTPRoute) bool {
	for _, finalizer := range route.Finalizers {
		if strings.HasPrefix(finalizer, config.routeBindingFinalizerPrefix()) {
			return true
		}
	}
	return false
}

func routeHasBindingAnnotations(config Config, route *gatewayv1.HTTPRoute) bool {
	for _, key := range []string{
		config.routeGatewayNamespaceAnnotation(),
		config.routeGatewayNameAnnotation(),
		config.routeGatewayUIDAnnotation(),
		config.routeClusterIDAnnotation(),
		config.routeProjectIDAnnotation(),
	} {
		if route.Annotations[key] != "" {
			return true
		}
	}
	return false
}

func routeHasControllerBinding(config Config, route *gatewayv1.HTTPRoute) bool {
	return controllerutil.ContainsFinalizer(route, config.routeFinalizer()) ||
		hasRouteBindingFinalizer(config, route) ||
		routeHasBindingAnnotations(config, route)
}
