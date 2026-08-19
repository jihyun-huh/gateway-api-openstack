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
	"strconv"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

type routeGraphResult struct {
	bindingRequired bool
	outcome         cloud.Outcome
}

func (r *HTTPRouteReconciler) ensureRouteGraph(
	ctx context.Context,
	expectedRoute *gatewayv1.HTTPRoute,
	expectedGateway *gatewayv1.Gateway,
) (routeGraphResult, error) {
	if r.APIReader == nil {
		return routeGraphResult{}, errAPIReaderRequired
	}
	release, err := acquireGatewayGraph(ctx, r.Coordinator, string(expectedGateway.UID))
	if err != nil {
		return routeGraphResult{}, fmt.Errorf("acquire Gateway graph: %w", err)
	}
	defer release()

	var route gatewayv1.HTTPRoute
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(expectedRoute), &route); err != nil {
		return routeGraphResult{}, errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if !sameHTTPRouteRevision(&route, expectedRoute.UID, expectedRoute.Generation, false) {
		return routeGraphResult{}, errHTTPRouteChanged
	}

	var gateway gatewayv1.Gateway
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(expectedGateway), &gateway); err != nil {
		return routeGraphResult{}, errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if gateway.UID != expectedGateway.UID || gateway.Generation != expectedGateway.Generation || !gateway.DeletionTimestamp.IsZero() {
		return routeGraphResult{}, errHTTPRouteChanged
	}
	var gatewayClass gatewayv1.GatewayClass
	if err := r.APIReader.Get(ctx, types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)}, &gatewayClass); err != nil {
		return routeGraphResult{}, errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if gatewayClass.Spec.ControllerName != r.Config.ControllerName || gatewayClass.Spec.ParametersRef != nil || len(route.Spec.ParentRefs) != 1 {
		return routeGraphResult{}, errHTTPRouteChanged
	}
	if !gatewayClassSupportsInstalledVersion(&gatewayClass) {
		return routeGraphResult{}, errUnsupportedGatewayAPIVersion
	}
	if !controllerutil.ContainsFinalizer(&gateway, r.Config.gatewayFinalizer()) {
		return routeGraphResult{}, errHTTPRouteChanged
	}
	if err := validateGatewayBinding(r.Config, &gateway); err != nil {
		return routeGraphResult{}, errHTTPRouteChanged
	}

	listener, validationErr := validateGateway(&gateway)
	if validationErr != nil {
		return routeGraphResult{}, nil
	}
	parent := managedRouteParent{ref: route.Spec.ParentRefs[0], gateway: &gateway, gatewayClass: &gatewayClass, listener: listener}
	if err := validateRouteParent(&route, parent); err != nil {
		return routeGraphResult{}, err
	}
	if gateway.Annotations[r.Config.gatewayListenerPortAnnotation()] != strconv.Itoa(int(listener.Port)) {
		return routeGraphResult{}, errHTTPRouteChanged
	}
	if structurallySupportedRoute(&route) {
		decision, err := r.evaluateRouteSlotWithReader(ctx, r.APIReader, &route)
		if err != nil {
			return routeGraphResult{}, err
		}
		if !decision.canReserve {
			return routeGraphResult{}, decision.rejection
		}
	}
	selected, err := r.isSelectedRouteWithReader(ctx, r.APIReader, &route, parent, false)
	if err != nil {
		return routeGraphResult{}, err
	}
	if !selected {
		return routeGraphResult{}, newRouteBuildError(routeErrorUnsupported, "Phase 1 supports one HTTPRoute per Gateway; an older route is already selected")
	}
	spec, err := r.buildRouteSpecWithReader(ctx, r.APIReader, &route, &gateway)
	if err != nil {
		return routeGraphResult{}, err
	}
	storedIdentity, bindingPresent, err := r.storedRouteIdentity(&route)
	if err != nil {
		return routeGraphResult{}, err
	}
	boundToCurrentGateway := bindingPresent &&
		storedIdentity == routeIdentity(r.Config, &gateway, &route) &&
		controllerutil.ContainsFinalizer(&route, r.Config.routeFinalizer())
	programmed := meta.FindStatusCondition(gateway.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	if (programmed == nil || programmed.Status != metav1.ConditionTrue || programmed.ObservedGeneration != gateway.Generation) &&
		!boundToCurrentGateway {
		return routeGraphResult{}, nil
	}
	desiredBinding := route.DeepCopy()
	r.applyRouteBinding(desiredBinding, &gateway)
	if !routeBindingMetadataEqual(&route, desiredBinding) {
		return routeGraphResult{bindingRequired: true}, nil
	}
	if err := r.validateRouteMutationOwnership(ctx, &route, &gateway, true); err != nil {
		return routeGraphResult{}, err
	}

	result, err := r.Provider.EnsureRoute(ctx, spec)
	if err != nil {
		return routeGraphResult{}, fmt.Errorf("ensure OpenStack resources for HTTPRoute: %w", err)
	}
	if err := result.Outcome.Validate(); err != nil {
		return routeGraphResult{}, fmt.Errorf("validate HTTPRoute provider outcome: %w", err)
	}
	return routeGraphResult{outcome: result.Outcome}, nil
}

func (r *HTTPRouteReconciler) validateRouteMutationOwnership(
	ctx context.Context,
	expectedRoute *gatewayv1.HTTPRoute,
	expectedGateway *gatewayv1.Gateway,
	requireRouteBinding bool,
) error {
	if err := validateInstalledGatewayAPIVersion(ctx, r.APIReader); err != nil {
		return err
	}
	var route gatewayv1.HTTPRoute
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(expectedRoute), &route); err != nil {
		return errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if !sameHTTPRouteRevision(
		&route,
		expectedRoute.UID,
		expectedRoute.Generation,
		!expectedRoute.DeletionTimestamp.IsZero(),
	) {
		return errHTTPRouteChanged
	}
	var gateway gatewayv1.Gateway
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(expectedGateway), &gateway); err != nil {
		return errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if gateway.UID != expectedGateway.UID || gateway.Generation != expectedGateway.Generation ||
		!gateway.DeletionTimestamp.IsZero() {
		return errHTTPRouteChanged
	}
	if !controllerutil.ContainsFinalizer(&gateway, r.Config.gatewayFinalizer()) ||
		!gatewayBindingMetadataEqual(r.Config, &gateway, expectedGateway) ||
		validateGatewayBinding(r.Config, &gateway) != nil {
		return errHTTPRouteChanged
	}
	if requireRouteBinding {
		stored, present, err := r.storedRouteIdentity(&route)
		if err != nil {
			return err
		}
		if !present || stored != routeIdentity(r.Config, &gateway, &route) ||
			!controllerutil.ContainsFinalizer(&route, r.Config.routeFinalizer()) {
			return errHTTPRouteChanged
		}
	}
	var gatewayClass gatewayv1.GatewayClass
	if err := r.APIReader.Get(
		ctx,
		types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)},
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
	return nil
}
