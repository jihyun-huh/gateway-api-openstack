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
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

type routeReconcileStatus struct {
	accepted          metav1.ConditionStatus
	acceptedReason    string
	acceptedMessage   string
	resolved          metav1.ConditionStatus
	resolvedReason    string
	resolvedMessage   string
	programmed        metav1.ConditionStatus
	programmedReason  string
	programmedMessage string
}

type parentStatusUpdate struct {
	parent gatewayv1.ParentReference
	status routeReconcileStatus
}

func (r *HTTPRouteReconciler) setRouteStatusesAndDetach(
	ctx context.Context,
	scope *httpRouteScope,
	updates []parentStatusUpdate,
) (ctrl.Result, error) {
	route := scope.route
	providerStatusPublished := routeHasProviderFailureStatus(route, updates, r.Config.ControllerName, r.Config.domain()+"/Programmed")
	checkpointNeeded, err := r.routeCleanupStatusCheckpointNeeded(ctx, scope, updates)
	if err != nil {
		return ctrl.Result{}, err
	}
	if checkpointNeeded {
		return ctrl.Result{Requeue: true}, nil
	}
	result, cleanupErr := r.detachRoute(ctx, route)
	if cleanupErr == nil {
		if providerStatusPublished {
			scope.setStatuses(updates)
		}
		return result, nil
	}
	if errors.Is(cleanupErr, errUnsupportedGatewayAPIVersion) {
		_, present, bindingErr := r.storedRouteIdentity(route)
		if bindingErr != nil {
			return ctrl.Result{}, bindingErr
		}
		if !present {
			return ctrl.Result{}, cleanupErr
		}
		return r.setRouteUnsupportedVersion(ctx, scope)
	}
	policy, ok := providerFailurePolicyFor(cleanupErr)
	if !ok {
		return ctrl.Result{}, cleanupErr
	}
	policy = providerCleanupFailurePolicy(policy, "HTTPRoute")
	diagnosticTransitioned, diagnosticErr := r.setRouteCleanupFailure(ctx, route, policy.reason)
	providerUpdates := routeProviderFailureUpdates(updates, policy)
	if len(providerUpdates) == 0 {
		scope.setStatuses(nil)
	} else {
		scope.setProviderFailureStatuses(providerUpdates, policy, true)
	}
	if diagnosticErr != nil {
		return ctrl.Result{}, errors.Join(diagnosticErr, safeProviderReconcileError(policy, cleanupErr))
	}
	scope.queueWarning(routeProviderWarning{
		policy:                 policy,
		action:                 "DeleteHTTPRoute",
		cleanupCheckpointSaved: diagnosticTransitioned,
		recordOnStatusChange:   len(providerUpdates) == 0,
	})
	if !policy.returnError {
		scope.patchCause = safeProviderReconcileError(policy, cleanupErr)
	}
	return providerFinalizationFailureResult(policy, cleanupErr, string(route.UID))
}

func routeHasProviderFailureStatus(
	route *gatewayv1.HTTPRoute,
	updates []parentStatusUpdate,
	controllerName gatewayv1.GatewayController,
	conditionType string,
) bool {
	if len(updates) == 0 {
		return false
	}
	for _, update := range updates {
		found := false
		for _, parent := range route.Status.Parents {
			if parent.ControllerName != controllerName || !parentRefsEqual(parent.ParentRef, route.Namespace, update.parent, route.Namespace) {
				continue
			}
			programmed := meta.FindStatusCondition(parent.Conditions, conditionType)
			if programmed != nil && programmed.Status != metav1.ConditionTrue && isProviderFailureReason(programmed.Reason) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for _, parent := range route.Status.Parents {
		if parent.ControllerName != controllerName {
			continue
		}
		planned := false
		for _, update := range updates {
			if parentRefsEqual(parent.ParentRef, route.Namespace, update.parent, route.Namespace) {
				planned = true
				break
			}
		}
		programmed := meta.FindStatusCondition(parent.Conditions, conditionType)
		if !planned || programmed == nil || programmed.Status == metav1.ConditionTrue || !isProviderFailureReason(programmed.Reason) {
			return false
		}
	}
	return true
}

func isProviderFailureReason(reason string) bool {
	switch reason {
	case providerReasonAuthenticationFailed, providerReasonAuthorizationFailed, providerReasonQuotaExceeded,
		providerReasonRateLimited, providerReasonRequestTimedOut, providerReasonUnavailable,
		providerReasonInvalidConfiguration, providerReasonResourceFailed, providerReasonOwnershipConflict:
		return true
	default:
		return false
	}
}

func routeProviderFailureUpdates(updates []parentStatusUpdate, policy providerFailurePolicy) []parentStatusUpdate {
	result := make([]parentStatusUpdate, len(updates))
	for index := range updates {
		result[index] = updates[index]
		result[index].status.programmed = policy.conditionStatus
		result[index].status.programmedReason = policy.reason
		result[index].status.programmedMessage = policy.message
	}
	return result
}

func (r *HTTPRouteReconciler) handleRouteProviderFailure(
	scope *httpRouteScope,
	parent gatewayv1.ParentReference,
	referencesEvaluated bool,
	providerErr error,
	action string,
) (ctrl.Result, error) {
	policy, ok := providerFailurePolicyFor(providerErr)
	if !ok {
		return ctrl.Result{}, providerErr
	}
	route := scope.route
	status := providerFailureRouteStatus(policy, referencesEvaluated)
	scope.setProviderFailureStatuses(
		[]parentStatusUpdate{{parent: parent, status: status}},
		policy,
		referencesEvaluated,
	)
	scope.queueWarning(routeProviderWarning{policy: policy, action: action})
	if !policy.returnError {
		scope.patchCause = safeProviderReconcileError(policy, providerErr)
	}
	return providerFailureResult(policy, providerErr, string(route.UID))
}

func (r *HTTPRouteReconciler) handleRouteFinalizationFailure(
	ctx context.Context,
	scope *httpRouteScope,
	identity cloud.Identity,
	providerErr error,
) (ctrl.Result, error) {
	policy, ok := providerFailurePolicyFor(providerErr)
	if !ok {
		return ctrl.Result{}, providerErr
	}
	route := scope.route
	policy = providerCleanupFailurePolicy(policy, "HTTPRoute")
	diagnosticTransitioned, diagnosticErr := r.setRouteCleanupFailure(ctx, route, policy.reason)
	parent, found := parentRefForStoredGateway(route, identity)
	if found {
		scope.setDeletingFailureStatus(parent, policy)
	}
	if diagnosticErr != nil {
		return ctrl.Result{}, errors.Join(
			safeProviderReconcileError(policy, providerErr),
			fmt.Errorf("publish HTTPRoute cleanup failure: %w", diagnosticErr),
		)
	}
	scope.queueWarning(routeProviderWarning{
		policy:                 policy,
		action:                 "DeleteHTTPRoute",
		cleanupCheckpointSaved: diagnosticTransitioned,
	})
	if !policy.returnError {
		scope.patchCause = safeProviderReconcileError(policy, providerErr)
	}
	return providerFinalizationFailureResult(policy, providerErr, string(route.UID))
}

func providerFailureRouteStatus(policy providerFailurePolicy, referencesEvaluated bool) routeReconcileStatus {
	status := programmedRouteStatus()
	if !referencesEvaluated {
		status.resolved = metav1.ConditionUnknown
		status.resolvedReason = string(gatewayv1.RouteReasonPending)
		status.resolvedMessage = "Backend references have not been evaluated"
	}
	status.programmed = policy.conditionStatus
	status.programmedReason = policy.reason
	status.programmedMessage = policy.message
	return status
}

func parentRefForStoredGateway(route *gatewayv1.HTTPRoute, identity cloud.Identity) (gatewayv1.ParentReference, bool) {
	parent := gatewayv1.ParentReference{Name: gatewayv1.ObjectName(identity.GatewayName)}
	if identity.GatewayNamespace != route.Namespace {
		namespace := gatewayv1.Namespace(identity.GatewayNamespace)
		parent.Namespace = &namespace
	}
	for _, candidate := range route.Spec.ParentRefs {
		if sameGatewayTarget(candidate, route.Namespace, parent, route.Namespace) {
			return candidate, true
		}
	}
	return gatewayv1.ParentReference{}, false
}

func statusForRouteBuildError(err error) routeReconcileStatus {
	if apierrors.IsNotFound(err) {
		return unresolvedRouteStatus(string(gatewayv1.RouteReasonBackendNotFound), err.Error())
	}
	var buildError *routeBuildError
	if errors.As(err, &buildError) {
		switch buildError.kind {
		case routeErrorInvalidKind:
			return unresolvedRouteStatus(string(gatewayv1.RouteReasonInvalidKind), buildError.message)
		case routeErrorRefNotPermitted:
			return unresolvedRouteStatus(string(gatewayv1.RouteReasonRefNotPermitted), buildError.message)
		case routeErrorUnsupportedProtocol:
			return unresolvedRouteStatus(string(gatewayv1.RouteReasonUnsupportedProtocol), buildError.message)
		case routeErrorNoMatchingParent:
			return rejectedRouteStatus(string(gatewayv1.RouteReasonNoMatchingParent), buildError.message)
		case routeErrorNotAllowed:
			return rejectedRouteStatus(string(gatewayv1.RouteReasonNotAllowedByListeners), buildError.message)
		case routeErrorPending:
			return pendingRouteStatus(buildError.message)
		default:
			return rejectedRouteStatus(string(gatewayv1.RouteReasonUnsupportedValue), buildError.message)
		}
	}
	if strings.Contains(err.Error(), "no ready endpoints") || strings.Contains(err.Error(), "no ready Nodes") {
		return pendingRouteStatus(err.Error())
	}
	return rejectedRouteStatus(string(gatewayv1.RouteReasonUnsupportedValue), err.Error())
}

func programmedRouteStatus() routeReconcileStatus {
	return routeReconcileStatus{
		accepted:          metav1.ConditionTrue,
		acceptedReason:    string(gatewayv1.RouteReasonAccepted),
		acceptedMessage:   "HTTPRoute is accepted by the Gateway",
		resolved:          metav1.ConditionTrue,
		resolvedReason:    string(gatewayv1.RouteReasonResolvedRefs),
		resolvedMessage:   "All backend references are resolved",
		programmed:        metav1.ConditionTrue,
		programmedReason:  "Programmed",
		programmedMessage: "Octavia pool, members, health monitor, and L7 policies are active",
	}
}

func pendingRouteStatus(message string) routeReconcileStatus {
	status := programmedRouteStatus()
	status.programmed = metav1.ConditionFalse
	status.programmedReason = "Pending"
	status.programmedMessage = message
	return status
}

func unsupportedGatewayAPIVersionRouteStatus() routeReconcileStatus {
	return routeReconcileStatus{
		accepted:          metav1.ConditionUnknown,
		acceptedReason:    string(gatewayv1.RouteReasonPending),
		acceptedMessage:   unsupportedGatewayAPIVersionMessage,
		resolved:          metav1.ConditionUnknown,
		resolvedReason:    string(gatewayv1.RouteReasonPending),
		resolvedMessage:   "Backend references were not evaluated",
		programmed:        metav1.ConditionFalse,
		programmedReason:  "Pending",
		programmedMessage: unsupportedGatewayAPIVersionMessage,
	}
}

func rejectedRouteStatus(reason, message string) routeReconcileStatus {
	return routeReconcileStatus{
		accepted:          metav1.ConditionFalse,
		acceptedReason:    reason,
		acceptedMessage:   message,
		resolved:          metav1.ConditionUnknown,
		resolvedReason:    string(gatewayv1.RouteReasonPending),
		resolvedMessage:   "Backend references were not evaluated because the route is unsupported",
		programmed:        metav1.ConditionFalse,
		programmedReason:  "Invalid",
		programmedMessage: message,
	}
}

func rejectedServiceProfileStatus(err error) routeReconcileStatus {
	status := statusForRouteBuildError(err)
	status.accepted = metav1.ConditionFalse
	status.acceptedReason = string(gatewayv1.RouteReasonUnsupportedValue)
	status.acceptedMessage = err.Error()
	return status
}

func unresolvedRouteStatus(reason, message string) routeReconcileStatus {
	status := programmedRouteStatus()
	status.resolved = metav1.ConditionFalse
	status.resolvedReason = reason
	status.resolvedMessage = message
	status.programmed = metav1.ConditionFalse
	status.programmedReason = "Invalid"
	status.programmedMessage = message
	return status
}

func (r *HTTPRouteReconciler) setRouteParentStatuses(ctx context.Context, route *gatewayv1.HTTPRoute, updates []parentStatusUpdate) error {
	plan := &routeStatusPlan{
		kind:    routeStatusPlanReplace,
		updates: append([]parentStatusUpdate(nil), updates...),
	}
	_, _, _, err := r.patchHTTPRouteStatus(ctx, route, plan)
	return err
}
