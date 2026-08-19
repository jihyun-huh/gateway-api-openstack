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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

func (r *GatewayReconciler) setGatewayFailure(gateway *gatewayv1.Gateway, validationErr error) {
	message := validationErr.Error()
	var detailed *gatewayValidationError
	errors.As(validationErr, &detailed)
	invalidRouteKinds := detailed != nil && detailed.invalidRouteKinds
	gatewayReason := gatewayv1.GatewayReasonListenersNotValid
	listenerSpecific := detailed == nil || detailed.gatewayReason == ""
	if detailed != nil {
		if detailed.gatewayReason != "" {
			gatewayReason = detailed.gatewayReason
		}
	}
	existingListeners := append([]gatewayv1.ListenerStatus(nil), gateway.Status.Listeners...)
	gateway.Status.Addresses = nil
	setCondition(&gateway.Status.Conditions, condition(string(gatewayv1.GatewayConditionAccepted), metav1.ConditionFalse, string(gatewayReason), message, gateway.Generation))
	setCondition(&gateway.Status.Conditions, condition(string(gatewayv1.GatewayConditionProgrammed), metav1.ConditionFalse, string(gatewayv1.GatewayReasonInvalid), message, gateway.Generation))
	gateway.Status.Listeners = make([]gatewayv1.ListenerStatus, 0, len(gateway.Spec.Listeners))
	for _, listener := range gateway.Spec.Listeners {
		listenerStatus := gatewayv1.ListenerStatus{
			Name:       listener.Name,
			Conditions: existingListenerConditionsFrom(existingListeners, listener.Name),
		}
		if !invalidRouteKinds || listenerAllowsHTTPRoute(listener) {
			listenerStatus.SupportedKinds = supportedHTTPRouteKinds()
		}
		acceptedStatus := metav1.ConditionFalse
		acceptedReason := string(gatewayv1.ListenerReasonUnsupportedValue)
		acceptedMessage := message
		programmedReason := string(gatewayv1.ListenerReasonInvalid)
		if !listenerSpecific {
			acceptedStatus = metav1.ConditionTrue
			acceptedReason = string(gatewayv1.ListenerReasonAccepted)
			acceptedMessage = "Listener configuration is accepted"
			programmedReason = string(gatewayv1.ListenerReasonPending)
		} else if listener.Protocol != gatewayv1.HTTPProtocolType {
			acceptedReason = string(gatewayv1.ListenerReasonUnsupportedProtocol)
		}
		setCondition(&listenerStatus.Conditions, condition(string(gatewayv1.ListenerConditionAccepted), acceptedStatus, acceptedReason, acceptedMessage, gateway.Generation))
		resolvedStatus := metav1.ConditionTrue
		resolvedReason := string(gatewayv1.ListenerReasonResolvedRefs)
		resolvedMessage := "Listener has no unresolved references"
		if invalidRouteKinds {
			resolvedStatus = metav1.ConditionFalse
			resolvedReason = string(gatewayv1.ListenerReasonInvalidRouteKinds)
			resolvedMessage = message
		}
		setCondition(&listenerStatus.Conditions, condition(string(gatewayv1.ListenerConditionResolvedRefs), resolvedStatus, resolvedReason, resolvedMessage, gateway.Generation))
		setCondition(&listenerStatus.Conditions, condition(string(gatewayv1.ListenerConditionConflicted), metav1.ConditionFalse, string(gatewayv1.ListenerReasonNoConflicts), "Listener has no conflicts", gateway.Generation))
		setCondition(&listenerStatus.Conditions, condition(string(gatewayv1.ListenerConditionProgrammed), metav1.ConditionFalse, programmedReason, message, gateway.Generation))
		gateway.Status.Listeners = append(gateway.Status.Listeners, listenerStatus)
	}
}

func supportedHTTPRouteKinds() []gatewayv1.RouteGroupKind {
	group := gatewayv1.Group(gatewayv1.GroupName)
	return []gatewayv1.RouteGroupKind{{Group: &group, Kind: gatewayv1.Kind("HTTPRoute")}}
}

func existingListenerConditions(gateway *gatewayv1.Gateway, listenerName gatewayv1.SectionName) []metav1.Condition {
	return existingListenerConditionsFrom(gateway.Status.Listeners, listenerName)
}

func existingListenerConditionsFrom(statuses []gatewayv1.ListenerStatus, listenerName gatewayv1.SectionName) []metav1.Condition {
	for _, status := range statuses {
		if status.Name == listenerName {
			return append([]metav1.Condition(nil), status.Conditions...)
		}
	}
	return nil
}

func listenerStatusesWithoutControllerConditions(gateway *gatewayv1.Gateway) []gatewayv1.ListenerStatus {
	statuses := make([]gatewayv1.ListenerStatus, 0, len(gateway.Status.Listeners))
	for _, existing := range gateway.Status.Listeners {
		conditions := make([]metav1.Condition, 0, len(existing.Conditions))
		for _, candidate := range existing.Conditions {
			if !isControllerOwnedListenerCondition(candidate.Type) {
				conditions = append(conditions, candidate)
			}
		}
		if len(conditions) != 0 {
			statuses = append(statuses, gatewayv1.ListenerStatus{Name: existing.Name, Conditions: conditions})
		}
	}
	return statuses
}

func isControllerOwnedListenerCondition(conditionType string) bool {
	switch conditionType {
	case string(gatewayv1.ListenerConditionAccepted),
		string(gatewayv1.ListenerConditionResolvedRefs),
		string(gatewayv1.ListenerConditionConflicted),
		string(gatewayv1.ListenerConditionProgrammed):
		return true
	default:
		return false
	}
}

func (r *GatewayReconciler) setGatewayFailureReason(gateway *gatewayv1.Gateway, reason gatewayv1.GatewayConditionReason, message string) {
	gateway.Status.Addresses = nil
	gateway.Status.Listeners = listenerStatusesWithoutControllerConditions(gateway)
	setCondition(&gateway.Status.Conditions, condition(string(gatewayv1.GatewayConditionAccepted), metav1.ConditionFalse, string(reason), message, gateway.Generation))
	setCondition(&gateway.Status.Conditions, condition(string(gatewayv1.GatewayConditionProgrammed), metav1.ConditionFalse, string(gatewayv1.GatewayReasonInvalid), message, gateway.Generation))
}

func (r *GatewayReconciler) setGatewayProgressing(ctx context.Context, gateway *gatewayv1.Gateway, listener *gatewayv1.Listener, message string) error {
	return r.setGatewayProvisioningStatus(ctx, gateway, listener, metav1.ConditionFalse, gatewayv1.GatewayReasonNoResources, gatewayv1.ListenerReasonPending, message, true)
}

func (r *GatewayReconciler) handleGatewayProviderFailure(
	ctx context.Context,
	scope *gatewayScope,
	listener *gatewayv1.Listener,
	providerErr error,
) (ctrl.Result, error) {
	policy, ok := providerFailurePolicyFor(providerErr)
	if !ok {
		return ctrl.Result{}, providerErr
	}
	gateway := scope.gateway
	gatewayReason, listenerReason := gatewayProviderFailureReasons(policy.category)
	programmedGeneration := conditionObservedGeneration(
		gateway.Status.Conditions,
		string(gatewayv1.GatewayConditionProgrammed),
		gateway.Generation,
		policy.advancesObservedGeneration,
	)
	transitioned := conditionTransitioned(
		gateway.Status.Conditions,
		string(gatewayv1.GatewayConditionProgrammed),
		policy.conditionStatus,
		string(gatewayReason),
		policy.message,
		programmedGeneration,
	)
	if err := r.setGatewayProvisioningStatus(
		ctx,
		gateway,
		listener,
		policy.conditionStatus,
		gatewayReason,
		listenerReason,
		policy.message,
		policy.advancesObservedGeneration,
	); err != nil {
		return ctrl.Result{}, errors.Join(safeProviderReconcileError(policy, providerErr), err)
	}
	if transitioned {
		scope.queueWarning(policy, "EnsureGateway")
	}
	if !policy.returnError {
		scope.patchCause = safeProviderReconcileError(policy, providerErr)
	}
	return providerFailureResult(policy, providerErr, string(gateway.UID))
}

func (r *GatewayReconciler) handleGatewayDeleteFailure(
	scope *gatewayScope,
	providerErr error,
) (ctrl.Result, error) {
	policy, ok := providerFailurePolicyFor(providerErr)
	if !ok {
		return ctrl.Result{}, providerErr
	}
	gateway := scope.gateway
	policy = providerCleanupFailurePolicy(policy, "Gateway")
	gatewayReason, listenerReason := gatewayProviderFailureReasons(policy.category)
	programmedGeneration := conditionObservedGeneration(
		gateway.Status.Conditions,
		string(gatewayv1.GatewayConditionProgrammed),
		gateway.Generation,
		policy.advancesObservedGeneration,
	)
	transitioned := conditionTransitioned(
		gateway.Status.Conditions,
		string(gatewayv1.GatewayConditionProgrammed),
		policy.conditionStatus,
		string(gatewayReason),
		policy.message,
		programmedGeneration,
	)
	r.setGatewayCleanupFailure(gateway, policy, gatewayReason, listenerReason)
	if transitioned {
		scope.queueWarning(policy, "DeleteGateway")
	}
	if !policy.returnError {
		scope.patchCause = safeProviderReconcileError(policy, providerErr)
	}
	return providerFinalizationFailureResult(policy, providerErr, string(gateway.UID))
}

func gatewayProviderFailureReasons(category cloud.ErrorCategory) (gatewayv1.GatewayConditionReason, gatewayv1.ListenerConditionReason) {
	switch category {
	case cloud.ErrorCategoryQuota:
		return gatewayv1.GatewayReasonNoResources, gatewayv1.ListenerReasonPending
	case cloud.ErrorCategoryTerminalValidation:
		return gatewayv1.GatewayReasonInvalid, gatewayv1.ListenerReasonInvalid
	case cloud.ErrorCategoryResourceFailure:
		return gatewayv1.GatewayConditionReason("ResourceFailed"), gatewayv1.ListenerConditionReason("ResourceFailed")
	case cloud.ErrorCategoryOwnershipConflict:
		return gatewayv1.GatewayConditionReason("OwnershipConflict"), gatewayv1.ListenerConditionReason("OwnershipConflict")
	default:
		return gatewayv1.GatewayReasonPending, gatewayv1.ListenerReasonPending
	}
}

func (r *GatewayReconciler) setGatewayCleanupFailure(
	gateway *gatewayv1.Gateway,
	policy providerFailurePolicy,
	gatewayReason gatewayv1.GatewayConditionReason,
	listenerReason gatewayv1.ListenerConditionReason,
) {
	programmedGeneration := conditionObservedGeneration(
		gateway.Status.Conditions,
		string(gatewayv1.GatewayConditionProgrammed),
		gateway.Generation,
		policy.advancesObservedGeneration,
	)
	setCondition(&gateway.Status.Conditions, condition(
		string(gatewayv1.GatewayConditionProgrammed),
		policy.conditionStatus,
		string(gatewayReason),
		policy.message,
		programmedGeneration,
	))
	for index := range gateway.Status.Listeners {
		listenerGeneration := conditionObservedGeneration(
			gateway.Status.Listeners[index].Conditions,
			string(gatewayv1.ListenerConditionProgrammed),
			gateway.Generation,
			policy.advancesObservedGeneration,
		)
		setCondition(&gateway.Status.Listeners[index].Conditions, condition(
			string(gatewayv1.ListenerConditionProgrammed),
			policy.conditionStatus,
			string(listenerReason),
			policy.message,
			listenerGeneration,
		))
	}
}

func (r *GatewayReconciler) setGatewayProvisioningStatus(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	listener *gatewayv1.Listener,
	programmed metav1.ConditionStatus,
	gatewayReason gatewayv1.GatewayConditionReason,
	listenerReason gatewayv1.ListenerConditionReason,
	message string,
	advanceProgrammedGeneration bool,
) error {
	attachedRoutes, err := r.attachedRouteCount(ctx, gateway, listener)
	if err != nil {
		return err
	}
	programmedGeneration := conditionObservedGeneration(
		gateway.Status.Conditions,
		string(gatewayv1.GatewayConditionProgrammed),
		gateway.Generation,
		advanceProgrammedGeneration,
	)
	setCondition(&gateway.Status.Conditions, condition(string(gatewayv1.GatewayConditionAccepted), metav1.ConditionTrue, string(gatewayv1.GatewayReasonAccepted), "Gateway configuration is accepted", gateway.Generation))
	setCondition(&gateway.Status.Conditions, condition(string(gatewayv1.GatewayConditionProgrammed), programmed, string(gatewayReason), message, programmedGeneration))
	listenerStatus := gatewayv1.ListenerStatus{
		Name:           listener.Name,
		SupportedKinds: supportedHTTPRouteKinds(),
		AttachedRoutes: attachedRoutes,
		Conditions:     existingListenerConditions(gateway, listener.Name),
	}
	setAcceptedListenerConditions(&listenerStatus.Conditions, gateway.Generation)
	listenerProgrammedGeneration := conditionObservedGeneration(
		listenerStatus.Conditions,
		string(gatewayv1.ListenerConditionProgrammed),
		gateway.Generation,
		advanceProgrammedGeneration,
	)
	setCondition(&listenerStatus.Conditions, condition(string(gatewayv1.ListenerConditionProgrammed), programmed, string(listenerReason), message, listenerProgrammedGeneration))
	gateway.Status.Listeners = []gatewayv1.ListenerStatus{listenerStatus}
	return nil
}

func (r *GatewayReconciler) setGatewayProgrammed(ctx context.Context, gateway *gatewayv1.Gateway, listener *gatewayv1.Listener, gatewayState cloud.GatewayState) error {
	attachedRoutes, err := r.attachedRouteCount(ctx, gateway, listener)
	if err != nil {
		return err
	}
	addressType := gatewayv1.IPAddressType
	gateway.Status.Addresses = []gatewayv1.GatewayStatusAddress{{Type: &addressType, Value: gatewayState.Address()}}
	setCondition(&gateway.Status.Conditions, condition(string(gatewayv1.GatewayConditionAccepted), metav1.ConditionTrue, string(gatewayv1.GatewayReasonAccepted), "Gateway configuration is accepted", gateway.Generation))
	setCondition(&gateway.Status.Conditions, condition(string(gatewayv1.GatewayConditionProgrammed), metav1.ConditionTrue, string(gatewayv1.GatewayReasonProgrammed), "Octavia load balancer and listener are active", gateway.Generation))
	listenerStatus := gatewayv1.ListenerStatus{
		Name:           listener.Name,
		SupportedKinds: supportedHTTPRouteKinds(),
		AttachedRoutes: attachedRoutes,
		Conditions:     existingListenerConditions(gateway, listener.Name),
	}
	setAcceptedListenerConditions(&listenerStatus.Conditions, gateway.Generation)
	setCondition(&listenerStatus.Conditions, condition(string(gatewayv1.ListenerConditionProgrammed), metav1.ConditionTrue, string(gatewayv1.ListenerReasonProgrammed), "Octavia listener is active", gateway.Generation))
	gateway.Status.Listeners = []gatewayv1.ListenerStatus{listenerStatus}
	return nil
}

func setAcceptedListenerConditions(conditions *[]metav1.Condition, generation int64) {
	setCondition(conditions, condition(string(gatewayv1.ListenerConditionAccepted), metav1.ConditionTrue, string(gatewayv1.ListenerReasonAccepted), "Listener is accepted", generation))
	setCondition(conditions, condition(string(gatewayv1.ListenerConditionResolvedRefs), metav1.ConditionTrue, string(gatewayv1.ListenerReasonResolvedRefs), "Listener references are resolved", generation))
	setCondition(conditions, condition(string(gatewayv1.ListenerConditionConflicted), metav1.ConditionFalse, string(gatewayv1.ListenerReasonNoConflicts), "Listener has no conflicts", generation))
}

func (r *GatewayReconciler) attachedRouteCount(ctx context.Context, gateway *gatewayv1.Gateway, listener *gatewayv1.Listener) (int32, error) {
	var routes gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routes, client.MatchingFields{
		indexHTTPRouteByStatusGateway: objectKeyString(client.ObjectKeyFromObject(gateway)),
	}); err != nil {
		return 0, err
	}
	var count int32
	for _, route := range routes.Items {
		for _, parentStatus := range route.Status.Parents {
			if !routeParentStatusTargetsListener(parentStatus, gateway, listener, r.Config.ControllerName) {
				continue
			}
			accepted := meta.FindStatusCondition(parentStatus.Conditions, string(gatewayv1.RouteConditionAccepted))
			if accepted != nil && accepted.Status == metav1.ConditionTrue && accepted.ObservedGeneration == route.Generation {
				count++
			}
			break
		}
	}
	return count, nil
}

func routeParentStatusTargetsListener(
	parentStatus gatewayv1.RouteParentStatus,
	gateway *gatewayv1.Gateway,
	listener *gatewayv1.Listener,
	controllerName gatewayv1.GatewayController,
) bool {
	parentRef := parentStatus.ParentRef
	if parentStatus.ControllerName != controllerName || !isGatewayParentRef(parentRef) || string(parentRef.Name) != gateway.Name {
		return false
	}
	if parentRef.Namespace != nil && string(*parentRef.Namespace) != gateway.Namespace {
		return false
	}
	if parentRef.SectionName != nil && *parentRef.SectionName != listener.Name {
		return false
	}
	return parentRef.Port == nil || *parentRef.Port == listener.Port
}
