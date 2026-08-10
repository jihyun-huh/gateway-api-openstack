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
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

type routeStatusPlanKind int

const (
	routeStatusPlanReplace routeStatusPlanKind = iota
	routeStatusPlanProviderFailure
	routeStatusPlanDeletingFailure
)

type routeStatusPlan struct {
	kind                routeStatusPlanKind
	updates             []parentStatusUpdate
	parent              gatewayv1.ParentReference
	policy              providerFailurePolicy
	referencesEvaluated bool
	validateOwnership   bool
}

type routeProviderWarning struct {
	policy                 providerFailurePolicy
	action                 string
	cleanupCheckpointSaved bool
	recordOnStatusChange   bool
}

type httpRouteScope struct {
	route      *gatewayv1.HTTPRoute
	statusPlan *routeStatusPlan
	warning    *routeProviderWarning
	patchCause error
	skipPatch  bool
}

func (r *HTTPRouteReconciler) newHTTPRouteScope(
	ctx context.Context,
	req ctrl.Request,
) (*httpRouteScope, bool, error) {
	route := &gatewayv1.HTTPRoute{}
	if err := r.Get(ctx, req.NamespacedName, route); err != nil {
		return nil, false, client.IgnoreNotFound(err)
	}
	scope := &httpRouteScope{route: route}
	if route.DeletionTimestamp.IsZero() {
		return scope, true, nil
	}
	responsible := controllerutil.ContainsFinalizer(route, r.Config.routeFinalizer()) ||
		r.hasRouteBindingFinalizer(route) || routeHasBindingAnnotations(r.Config, route)
	return scope, responsible, nil
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

func (s *httpRouteScope) setStatuses(updates []parentStatusUpdate) {
	s.statusPlan = &routeStatusPlan{
		kind:              routeStatusPlanReplace,
		updates:           append([]parentStatusUpdate(nil), updates...),
		validateOwnership: true,
	}
}

func (s *httpRouteScope) setProviderFailureStatuses(
	updates []parentStatusUpdate,
	policy providerFailurePolicy,
	referencesEvaluated bool,
) {
	s.statusPlan = &routeStatusPlan{
		kind:                routeStatusPlanProviderFailure,
		updates:             append([]parentStatusUpdate(nil), updates...),
		policy:              policy,
		referencesEvaluated: referencesEvaluated,
		validateOwnership:   true,
	}
}

func (s *httpRouteScope) setDeletingFailureStatus(
	parent gatewayv1.ParentReference,
	policy providerFailurePolicy,
) {
	s.statusPlan = &routeStatusPlan{
		kind:   routeStatusPlanDeletingFailure,
		parent: parent,
		policy: policy,
	}
}

func (s *httpRouteScope) queueWarning(warning routeProviderWarning) {
	s.warning = &warning
}

func (r *HTTPRouteReconciler) patchHTTPRoute(ctx context.Context, scope *httpRouteScope) error {
	if scope.skipPatch {
		return nil
	}
	applied, changed, transitioned := true, false, false
	if scope.statusPlan != nil {
		var err error
		applied, changed, transitioned, err = r.patchHTTPRouteStatus(ctx, scope.route, scope.statusPlan)
		if err != nil {
			return fmt.Errorf("patch HTTPRoute status: %w", err)
		}
	}
	if applied && scope.warning != nil &&
		(scope.warning.cleanupCheckpointSaved || transitioned || scope.warning.recordOnStatusChange && changed) {
		recordProviderWarning(r.Recorder, scope.route, scope.warning.policy, scope.warning.action)
	}
	return nil
}

func (r *HTTPRouteReconciler) patchHTTPRouteStatus(
	ctx context.Context,
	route *gatewayv1.HTTPRoute,
	plan *routeStatusPlan,
) (applied, changed, transitioned bool, err error) {
	key := client.ObjectKeyFromObject(route)
	uid, generation := route.UID, route.Generation
	isDeleting := !route.DeletionTimestamp.IsZero()
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		applied, changed, transitioned = false, false, false
		current := &gatewayv1.HTTPRoute{}
		if err := r.routePatchReader().Get(ctx, key, current); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if !sameHTTPRouteRevision(current, uid, generation, isDeleting) {
			return errHTTPRouteChanged
		}
		if plan.validateOwnership {
			if err := r.validateRouteStatusOwnership(ctx, current, plan.updates); err != nil {
				return err
			}
		}

		base := current.DeepCopy()
		transitioned = plan.apply(r.Config, current)
		applied = true
		if reflect.DeepEqual(base.Status, current.Status) {
			*route = *current
			return nil
		}
		if err := r.Status().Patch(ctx, current, optimisticMergeFrom(base)); err != nil {
			return err
		}
		changed = true
		*route = *current
		log.FromContext(ctx).V(1).Info("Updated HTTPRoute status")
		return nil
	})
	return applied, changed, transitioned, err
}

func (r *HTTPRouteReconciler) routeStatusCheckpointNeeded(
	ctx context.Context,
	scope *httpRouteScope,
) (bool, error) {
	if scope.statusPlan == nil {
		return false, nil
	}
	current := &gatewayv1.HTTPRoute{}
	if err := r.routePatchReader().Get(ctx, client.ObjectKeyFromObject(scope.route), current); err != nil {
		return false, fmt.Errorf("read HTTPRoute status checkpoint: %w", err)
	}
	if !sameHTTPRouteRevision(
		current,
		scope.route.UID,
		scope.route.Generation,
		!scope.route.DeletionTimestamp.IsZero(),
	) {
		return false, errHTTPRouteChanged
	}
	if scope.statusPlan.validateOwnership {
		if err := r.validateRouteStatusOwnership(ctx, current, scope.statusPlan.updates); err != nil {
			return false, err
		}
	}
	desired := current.DeepCopy()
	scope.statusPlan.apply(r.Config, desired)
	*scope.route = *current
	return !reflect.DeepEqual(current.Status, desired.Status), nil
}

func (r *HTTPRouteReconciler) routeProviderFailureCheckpointExists(
	ctx context.Context,
	scope *httpRouteScope,
	updates []parentStatusUpdate,
) (bool, error) {
	current := &gatewayv1.HTTPRoute{}
	if err := r.routePatchReader().Get(ctx, client.ObjectKeyFromObject(scope.route), current); err != nil {
		return false, fmt.Errorf("read HTTPRoute provider failure checkpoint: %w", err)
	}
	if !sameHTTPRouteRevision(
		current,
		scope.route.UID,
		scope.route.Generation,
		!scope.route.DeletionTimestamp.IsZero(),
	) {
		return false, errHTTPRouteChanged
	}
	*scope.route = *current
	return routeHasProviderFailureStatus(
		current,
		updates,
		r.Config.ControllerName,
		r.Config.domain()+"/Programmed",
	), nil
}

func (r *HTTPRouteReconciler) routeCleanupStatusCheckpointNeeded(
	ctx context.Context,
	scope *httpRouteScope,
	updates []parentStatusUpdate,
) (bool, error) {
	if routeHasProviderFailureStatus(
		scope.route,
		updates,
		r.Config.ControllerName,
		r.Config.domain()+"/Programmed",
	) {
		durable, err := r.routeProviderFailureCheckpointExists(ctx, scope, updates)
		if err != nil {
			return false, err
		}
		if durable {
			return false, nil
		}
	}
	scope.setStatuses(updates)
	return r.routeStatusCheckpointNeeded(ctx, scope)
}

func (r *HTTPRouteReconciler) validateRouteStatusOwnership(
	ctx context.Context,
	route *gatewayv1.HTTPRoute,
	updates []parentStatusUpdate,
) error {
	parents, err := r.managedParentsWithReader(ctx, r.routePatchReader(), route)
	if err != nil {
		return err
	}
	for _, update := range updates {
		owned := false
		for _, parent := range parents {
			if parentRefsEqual(parent.ref, route.Namespace, update.parent, route.Namespace) {
				owned = true
				break
			}
		}
		if !owned {
			return errHTTPRouteChanged
		}
	}
	for _, parent := range parents {
		planned := false
		for _, update := range updates {
			if parentRefsEqual(parent.ref, route.Namespace, update.parent, route.Namespace) {
				planned = true
				break
			}
		}
		if !planned {
			return errHTTPRouteChanged
		}
	}
	return nil
}

func (r *HTTPRouteReconciler) routePatchReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (p *routeStatusPlan) apply(config Config, route *gatewayv1.HTTPRoute) bool {
	switch p.kind {
	case routeStatusPlanReplace:
		applyRouteParentStatuses(config, route, p.updates)
		return false
	case routeStatusPlanProviderFailure:
		return applyRouteProviderFailureStatuses(
			config,
			route,
			p.updates,
			p.policy,
			p.referencesEvaluated,
		)
	case routeStatusPlanDeletingFailure:
		return applyDeletingRouteProviderFailure(config, route, p.parent, p.policy)
	default:
		return false
	}
}

func applyRouteParentStatuses(config Config, route *gatewayv1.HTTPRoute, updates []parentStatusUpdate) {
	existing := append([]gatewayv1.RouteParentStatus(nil), route.Status.Parents...)
	parents := make([]gatewayv1.RouteParentStatus, 0, len(existing)+len(updates))
	for _, parent := range existing {
		if parent.ControllerName != config.ControllerName {
			parents = append(parents, parent)
		}
	}
	for index, update := range updates {
		if parentStatusUpdateAppearedEarlier(route.Namespace, updates, index) {
			continue
		}
		parentStatus := routeParentStatus(route, config.ControllerName, existing, update.parent)
		setCondition(&parentStatus.Conditions, condition(string(gatewayv1.RouteConditionAccepted), update.status.accepted, update.status.acceptedReason, update.status.acceptedMessage, route.Generation))
		setCondition(&parentStatus.Conditions, condition(string(gatewayv1.RouteConditionResolvedRefs), update.status.resolved, update.status.resolvedReason, update.status.resolvedMessage, route.Generation))
		setCondition(&parentStatus.Conditions, condition(config.domain()+"/Programmed", update.status.programmed, update.status.programmedReason, update.status.programmedMessage, route.Generation))
		parents = append(parents, parentStatus)
	}
	route.Status.Parents = parents
}

func applyRouteProviderFailureStatuses(
	config Config,
	route *gatewayv1.HTTPRoute,
	updates []parentStatusUpdate,
	policy providerFailurePolicy,
	referencesEvaluated bool,
) bool {
	existing := append([]gatewayv1.RouteParentStatus(nil), route.Status.Parents...)
	parents := make([]gatewayv1.RouteParentStatus, 0, len(existing)+len(updates))
	for _, parent := range existing {
		if parent.ControllerName != config.ControllerName {
			parents = append(parents, parent)
		}
	}
	transitioned := false
	for index, update := range updates {
		if parentStatusUpdateAppearedEarlier(route.Namespace, updates, index) {
			continue
		}
		parentStatus := routeParentStatus(route, config.ControllerName, existing, update.parent)
		resolvedGeneration := conditionObservedGeneration(
			parentStatus.Conditions,
			string(gatewayv1.RouteConditionResolvedRefs),
			route.Generation,
			referencesEvaluated,
		)
		programmedGeneration := conditionObservedGeneration(
			parentStatus.Conditions,
			config.domain()+"/Programmed",
			route.Generation,
			policy.advancesObservedGeneration,
		)
		transitioned = transitioned || conditionTransitioned(
			parentStatus.Conditions,
			config.domain()+"/Programmed",
			update.status.programmed,
			update.status.programmedReason,
			update.status.programmedMessage,
			programmedGeneration,
		)
		setCondition(&parentStatus.Conditions, condition(string(gatewayv1.RouteConditionAccepted), update.status.accepted, update.status.acceptedReason, update.status.acceptedMessage, route.Generation))
		setCondition(&parentStatus.Conditions, condition(string(gatewayv1.RouteConditionResolvedRefs), update.status.resolved, update.status.resolvedReason, update.status.resolvedMessage, resolvedGeneration))
		setCondition(&parentStatus.Conditions, condition(config.domain()+"/Programmed", update.status.programmed, update.status.programmedReason, update.status.programmedMessage, programmedGeneration))
		parents = append(parents, parentStatus)
	}
	route.Status.Parents = parents
	return transitioned
}

func parentStatusUpdateAppearedEarlier(
	routeNamespace string,
	updates []parentStatusUpdate,
	index int,
) bool {
	for earlier := 0; earlier < index; earlier++ {
		if parentRefsEqual(
			updates[earlier].parent,
			routeNamespace,
			updates[index].parent,
			routeNamespace,
		) {
			return true
		}
	}
	return false
}

func applyDeletingRouteProviderFailure(
	config Config,
	route *gatewayv1.HTTPRoute,
	parent gatewayv1.ParentReference,
	policy providerFailurePolicy,
) bool {
	parentInSpec := false
	for _, candidate := range route.Spec.ParentRefs {
		if parentRefsEqual(candidate, route.Namespace, parent, route.Namespace) {
			parentInSpec = true
			break
		}
	}
	if !parentInSpec {
		return false
	}
	for index := range route.Status.Parents {
		candidate := &route.Status.Parents[index]
		if candidate.ControllerName != config.ControllerName ||
			!parentRefsEqual(candidate.ParentRef, route.Namespace, parent, route.Namespace) {
			continue
		}
		programmedGeneration := conditionObservedGeneration(
			candidate.Conditions,
			config.domain()+"/Programmed",
			route.Generation,
			policy.advancesObservedGeneration,
		)
		transitioned := conditionTransitioned(
			candidate.Conditions,
			config.domain()+"/Programmed",
			policy.conditionStatus,
			policy.reason,
			policy.message,
			programmedGeneration,
		)
		setCondition(&candidate.Conditions, condition(
			config.domain()+"/Programmed",
			policy.conditionStatus,
			policy.reason,
			policy.message,
			programmedGeneration,
		))
		return transitioned
	}
	return false
}

func routeParentStatus(
	route *gatewayv1.HTTPRoute,
	controllerName gatewayv1.GatewayController,
	existing []gatewayv1.RouteParentStatus,
	parent gatewayv1.ParentReference,
) gatewayv1.RouteParentStatus {
	status := gatewayv1.RouteParentStatus{ParentRef: parent, ControllerName: controllerName}
	for _, candidate := range existing {
		if candidate.ControllerName != controllerName ||
			!parentRefsEqual(candidate.ParentRef, route.Namespace, parent, route.Namespace) {
			continue
		}
		status.Conditions = append([]metav1.Condition(nil), candidate.Conditions...)
		break
	}
	return status
}
