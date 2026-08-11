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
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

// GatewayReconciler owns the Gateway-scoped Octavia resources.
type GatewayReconciler struct {
	client.Client
	Provider    cloud.Provider
	Coordinator *GraphCoordinator
	APIReader   client.Reader
	Recorder    events.EventRecorder
	Config      Config
}

const openStackResourceNamePrefix = "gateway-api-openstack"

var (
	errGatewayChanged    = errors.New("Gateway changed during reconciliation")
	errAPIReaderRequired = errors.New("uncached API reader is required")
)

type gatewayValidationError struct {
	message           string
	invalidRouteKinds bool
	gatewayReason     gatewayv1.GatewayConditionReason
}

func (e *gatewayValidationError) Error() string { return e.message }

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses;httproutes,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch;update
func (r *GatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("gateway", req.NamespacedName)
	ctx = log.IntoContext(ctx, logger)
	logger.V(4).Info("Reconciling Gateway")

	var gateway gatewayv1.Gateway
	if err := r.Get(ctx, req.NamespacedName, &gateway); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !gateway.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&gateway, r.Config.gatewayFinalizer()) {
			return ctrl.Result{}, nil
		}
		result, err := r.cleanupGateway(ctx, client.ObjectKeyFromObject(&gateway), gateway.ResourceVersion)
		if err != nil {
			return result, err
		}
		if result.RequeueAfter > 0 {
			return result, nil
		}
		logger.V(1).Info("Cleaned up OpenStack resources for Gateway")
		return ctrl.Result{}, nil
	}

	return r.reconcile(ctx, gateway)
}

func (r *GatewayReconciler) reconcile(ctx context.Context, gateway gatewayv1.Gateway) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	gatewayClass, isManaged, err := r.getGatewayClass(ctx, &gateway)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !isManaged {
		if controllerutil.ContainsFinalizer(&gateway, r.Config.gatewayFinalizer()) {
			result, err := r.cleanupGateway(ctx, client.ObjectKeyFromObject(&gateway), gateway.ResourceVersion)
			if err != nil {
				return result, err
			}
			if result.RequeueAfter > 0 {
				return result, nil
			}
			logger.V(1).Info("Cleaned up OpenStack resources for unmanaged Gateway")
		}
		return ctrl.Result{}, nil
	}

	if err := validateGatewayBinding(r.Config, &gateway); err != nil {
		return ctrl.Result{}, r.setGatewayFailureReason(ctx, &gateway, gatewayv1.GatewayReasonInvalidParameters, err.Error())
	}
	if gatewayClass != nil && gatewayClass.Spec.ParametersRef != nil {
		if err := r.setGatewayFailureReason(ctx, &gateway, gatewayv1.GatewayReasonInvalidParameters, "GatewayClass parametersRef is not supported in Phase 1"); err != nil {
			return ctrl.Result{}, err
		}
		return r.cleanupGateway(ctx, client.ObjectKeyFromObject(&gateway), gateway.ResourceVersion)
	}

	gatewayListener, validationErr := validateGateway(&gateway)
	if validationErr != nil {
		if err := r.setGatewayFailure(ctx, &gateway, validationErr); err != nil {
			return ctrl.Result{}, err
		}
		return r.cleanupGateway(ctx, client.ObjectKeyFromObject(&gateway), gateway.ResourceVersion)
	}
	desiredListenerPort := strconv.Itoa(int(gatewayListener.Port))
	storedListenerPort := gateway.Annotations[r.Config.gatewayListenerPortAnnotation()]
	if storedListenerPort != "" && storedListenerPort != desiredListenerPort {
		if err := r.setGatewayProgressing(ctx, &gateway, gatewayListener, "Listener port changed; replacing the controller-owned OpenStack resource graph"); err != nil {
			return ctrl.Result{}, err
		}
		return r.cleanupGateway(ctx, client.ObjectKeyFromObject(&gateway), gateway.ResourceVersion)
	}
	storedClusterID := gateway.Annotations[r.Config.gatewayClusterIDAnnotation()]
	storedOpenStackProjectID := gateway.Annotations[r.Config.gatewayProjectIDAnnotation()]
	if !controllerutil.ContainsFinalizer(&gateway, r.Config.gatewayFinalizer()) || storedListenerPort == "" || storedClusterID == "" || storedOpenStackProjectID == "" {
		metadataBase := gateway.DeepCopy()
		if gateway.Annotations == nil {
			gateway.Annotations = map[string]string{}
		}
		gateway.Annotations[r.Config.gatewayListenerPortAnnotation()] = desiredListenerPort
		gateway.Annotations[r.Config.gatewayClusterIDAnnotation()] = r.Config.ClusterID
		gateway.Annotations[r.Config.gatewayProjectIDAnnotation()] = r.Config.OpenStackProjectID
		controllerutil.AddFinalizer(&gateway, r.Config.gatewayFinalizer())
		if err := r.Patch(ctx, &gateway, optimisticMergeFrom(metadataBase)); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch Gateway resource binding: %w", err)
		}
		logger.V(1).Info("Added finalizer and OpenStack resource binding to Gateway")
		return ctrl.Result{Requeue: true}, nil
	}
	gatewayResult, err := r.ensureGateway(ctx, &gateway)
	if err != nil {
		if errors.Is(err, errGatewayChanged) {
			return ctrl.Result{}, err
		}
		return r.handleGatewayProviderFailure(ctx, &gateway, gatewayListener, err)
	}
	if gatewayResult.Outcome.State == cloud.OutcomeProgressing {
		message := providerProgressMessage(gatewayResult.Outcome, "Octavia resources for the Gateway are still progressing")
		if err := r.setGatewayProgressing(ctx, &gateway, gatewayListener, message); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: providerProgressRequeueAfter(gatewayResult.Outcome, gateway.UID)}, nil
	}
	gatewayState := gatewayResult.State
	logger.V(1).Info("Ensured OpenStack resources for Gateway", "loadBalancerID", gatewayState.LoadBalancerID, "listenerID", gatewayState.ListenerID)
	if err := r.setGatewayProgrammed(ctx, &gateway, gatewayListener, gatewayState); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: openStackResyncAfter(r.Config.OpenStackResyncInterval, gateway.UID)}, nil
}

// cleanupGateway converges a previously valid Gateway to no OpenStack
// resources before dropping this controller's finalizer. DeleteGateway is
// identity-safe and idempotent, so it is also safe when creation never began.
func (r *GatewayReconciler) cleanupGateway(ctx context.Context, gatewayKey types.NamespacedName, expectedResourceVersion string) (ctrl.Result, error) {
	var gateway gatewayv1.Gateway
	if err := r.Get(ctx, gatewayKey, &gateway); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if expectedResourceVersion != "" && gateway.ResourceVersion != expectedResourceVersion {
		return ctrl.Result{}, apierrors.NewConflict(
			schema.GroupResource{Group: gatewayv1.GroupName, Resource: "gateways"},
			gateway.Name,
			errors.New("gateway changed before OpenStack resource cleanup"),
		)
	}
	hasFinalizer := controllerutil.ContainsFinalizer(&gateway, r.Config.gatewayFinalizer())
	_, hasListenerPortBinding := gateway.Annotations[r.Config.gatewayListenerPortAnnotation()]
	_, hasClusterIDBinding := gateway.Annotations[r.Config.gatewayClusterIDAnnotation()]
	_, hasProjectIDBinding := gateway.Annotations[r.Config.gatewayProjectIDAnnotation()]
	hasBinding := hasListenerPortBinding || hasClusterIDBinding || hasProjectIDBinding
	if !hasFinalizer && !hasBinding {
		return ctrl.Result{}, nil
	}
	if err := validateGatewayBinding(r.Config, &gateway); err != nil {
		return ctrl.Result{}, err
	}
	outcome, err := r.deleteGateway(ctx, &gateway)
	if err != nil {
		providerErr := fmt.Errorf("delete OpenStack resources for Gateway: %w", err)
		return r.handleGatewayDeleteFailure(ctx, &gateway, providerErr)
	}
	if err := outcome.Validate(); err != nil {
		return ctrl.Result{}, fmt.Errorf("validate Gateway deletion outcome: %w", err)
	}
	if outcome.State == cloud.OutcomeProgressing {
		return ctrl.Result{RequeueAfter: providerProgressRequeueAfter(outcome, gateway.UID)}, nil
	}
	metadataBase := gateway.DeepCopy()
	controllerutil.RemoveFinalizer(&gateway, r.Config.gatewayFinalizer())
	delete(gateway.Annotations, r.Config.gatewayListenerPortAnnotation())
	delete(gateway.Annotations, r.Config.gatewayClusterIDAnnotation())
	delete(gateway.Annotations, r.Config.gatewayProjectIDAnnotation())
	if err := r.Patch(ctx, &gateway, optimisticMergeFrom(metadataBase)); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("remove Gateway resource binding: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *GatewayReconciler) ensureGateway(ctx context.Context, expected *gatewayv1.Gateway) (cloud.GatewayResult, error) {
	release, err := acquireGatewayGraph(ctx, r.Coordinator, string(expected.UID))
	if err != nil {
		return cloud.GatewayResult{}, fmt.Errorf("acquire Gateway graph: %w", err)
	}
	defer release()

	var current gatewayv1.Gateway
	if err := r.Get(ctx, client.ObjectKeyFromObject(expected), &current); err != nil {
		return cloud.GatewayResult{}, errors.Join(errGatewayChanged, client.IgnoreNotFound(err))
	}
	if current.UID != expected.UID || current.Generation != expected.Generation || !current.DeletionTimestamp.IsZero() {
		return cloud.GatewayResult{}, errGatewayChanged
	}
	gatewayClass, managed, err := r.getGatewayClass(ctx, &current)
	if err != nil {
		return cloud.GatewayResult{}, err
	}
	if !managed || gatewayClass == nil || gatewayClass.Spec.ParametersRef != nil {
		return cloud.GatewayResult{}, errGatewayChanged
	}
	if !controllerutil.ContainsFinalizer(&current, r.Config.gatewayFinalizer()) {
		return cloud.GatewayResult{}, errGatewayChanged
	}
	if err := validateGatewayBinding(r.Config, &current); err != nil {
		return cloud.GatewayResult{}, errors.Join(errGatewayChanged, err)
	}
	listener, validationErr := validateGateway(&current)
	if validationErr != nil || current.Annotations[r.Config.gatewayListenerPortAnnotation()] != strconv.Itoa(int(listener.Port)) {
		return cloud.GatewayResult{}, errGatewayChanged
	}

	result, err := r.Provider.EnsureGateway(ctx, r.gatewaySpec(&current, listener))
	if err != nil {
		return cloud.GatewayResult{}, err
	}
	if err := result.Outcome.Validate(); err != nil {
		return cloud.GatewayResult{}, fmt.Errorf("validate Gateway provider outcome: %w", err)
	}
	return result, nil
}

func (r *GatewayReconciler) deleteGateway(ctx context.Context, expected *gatewayv1.Gateway) (cloud.Outcome, error) {
	if r.APIReader == nil {
		return cloud.Outcome{}, errAPIReaderRequired
	}
	release, err := acquireGatewayGraph(ctx, r.Coordinator, string(expected.UID))
	if err != nil {
		return cloud.Outcome{}, fmt.Errorf("acquire Gateway graph: %w", err)
	}
	defer release()

	var current gatewayv1.Gateway
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(expected), &current); err != nil {
		return cloud.Outcome{}, errors.Join(errGatewayChanged, client.IgnoreNotFound(err))
	}
	if current.UID != expected.UID || current.Generation != expected.Generation ||
		current.ResourceVersion != expected.ResourceVersion || current.DeletionTimestamp.IsZero() != expected.DeletionTimestamp.IsZero() {
		return cloud.Outcome{}, errGatewayChanged
	}
	if err := validateGatewayBinding(r.Config, &current); err != nil {
		return cloud.Outcome{}, errors.Join(errGatewayChanged, err)
	}

	return r.Provider.DeleteGateway(ctx, r.storedGatewayIdentity(&current))
}

func validateGatewayBinding(config Config, gateway *gatewayv1.Gateway) error {
	bindingValues := []string{
		gateway.Annotations[config.gatewayListenerPortAnnotation()],
		gateway.Annotations[config.gatewayClusterIDAnnotation()],
		gateway.Annotations[config.gatewayProjectIDAnnotation()],
	}
	populatedBindingCount := 0
	for _, value := range bindingValues {
		if value != "" {
			populatedBindingCount++
		}
	}
	if populatedBindingCount != len(bindingValues) && (populatedBindingCount != 0 || controllerutil.ContainsFinalizer(gateway, config.gatewayFinalizer())) {
		return fmt.Errorf("gateway %s/%s has an incomplete stored OpenStack resource binding; restore the original annotations before reconciling", gateway.Namespace, gateway.Name)
	}
	if stored := gateway.Annotations[config.gatewayClusterIDAnnotation()]; stored != "" && stored != config.ClusterID {
		return fmt.Errorf("controller cluster identity differs from this Gateway's existing resource binding; restore the original cluster ID before reconciling")
	}
	if stored := gateway.Annotations[config.gatewayProjectIDAnnotation()]; stored != "" && stored != config.OpenStackProjectID {
		return fmt.Errorf("authenticated OpenStack project differs from this Gateway's existing resource binding; restore access to the original project before reconciling")
	}
	return nil
}

func (r *GatewayReconciler) storedGatewayIdentity(gateway *gatewayv1.Gateway) cloud.Identity {
	identity := gatewayIdentity(r.Config, gateway)
	if value := gateway.Annotations[r.Config.gatewayClusterIDAnnotation()]; value != "" {
		identity.ClusterID = value
	}
	if value := gateway.Annotations[r.Config.gatewayProjectIDAnnotation()]; value != "" {
		identity.OpenStackProjectID = value
	}
	return identity
}

func (r *GatewayReconciler) getGatewayClass(ctx context.Context, gateway *gatewayv1.Gateway) (*gatewayv1.GatewayClass, bool, error) {
	var gatewayClass gatewayv1.GatewayClass
	key := types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)}
	if err := r.Get(ctx, key, &gatewayClass); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get GatewayClass %q: %w", key.Name, err)
	}
	return &gatewayClass, gatewayClass.Spec.ControllerName == r.Config.ControllerName, nil
}

func validateGateway(gateway *gatewayv1.Gateway) (*gatewayv1.Listener, error) {
	if len(gateway.Spec.Addresses) != 0 {
		return nil, &gatewayValidationError{
			message:       "spec.addresses is not supported in Phase 1",
			gatewayReason: gatewayv1.GatewayReasonUnsupportedAddress,
		}
	}
	if gateway.Spec.Infrastructure != nil {
		return nil, &gatewayValidationError{
			message:       "spec.infrastructure is not supported in Phase 1",
			gatewayReason: gatewayv1.GatewayReasonInvalidParameters,
		}
	}
	if gateway.Spec.AllowedListeners != nil {
		return nil, &gatewayValidationError{
			message:       "spec.allowedListeners and ListenerSet attachment are not supported in Phase 1",
			gatewayReason: gatewayv1.GatewayReasonInvalidParameters,
		}
	}
	if gateway.Spec.TLS != nil {
		return nil, &gatewayValidationError{
			message:       "gateway-wide spec.tls is not supported in Phase 1",
			gatewayReason: gatewayv1.GatewayReasonInvalidParameters,
		}
	}
	if len(gateway.Spec.Listeners) != 1 {
		return nil, fmt.Errorf("exactly one listener is supported in Phase 1")
	}
	listener := &gateway.Spec.Listeners[0]
	if listener.Protocol != gatewayv1.HTTPProtocolType || listener.TLS != nil {
		return nil, fmt.Errorf("only a plain HTTP listener is supported in Phase 1")
	}
	if listener.Hostname != nil {
		return nil, fmt.Errorf("listener hostname is not supported in Phase 1; put one exact hostname on HTTPRoute")
	}
	if listener.AllowedRoutes != nil && listener.AllowedRoutes.Namespaces != nil &&
		listener.AllowedRoutes.Namespaces.From != nil &&
		*listener.AllowedRoutes.Namespaces.From != gatewayv1.NamespacesFromSame {
		return nil, fmt.Errorf("only same-namespace HTTPRoutes are supported in Phase 1")
	}
	if listener.AllowedRoutes != nil && len(listener.AllowedRoutes.Kinds) != 0 {
		for _, kind := range listener.AllowedRoutes.Kinds {
			if !isHTTPRouteGroupKind(kind) {
				return nil, &gatewayValidationError{
					message:           "only HTTPRoute is supported in listener allowedRoutes.kinds in Phase 1",
					invalidRouteKinds: true,
				}
			}
		}
	}
	return listener, nil
}

func (r *GatewayReconciler) gatewaySpec(gateway *gatewayv1.Gateway, listener *gatewayv1.Listener) cloud.GatewaySpec {
	return cloud.GatewaySpec{
		Identity:          gatewayIdentity(r.Config, gateway),
		Provider:          r.Config.Provider,
		VIPSubnetID:       r.Config.VIPSubnetID,
		ExternalNetworkID: r.Config.ExternalNetworkID,
		ListenerName:      resourceDisplayName(gateway.Namespace, gateway.Name, string(listener.Name)),
		ListenerPort:      int(listener.Port),
	}
}

func (r *GatewayReconciler) setGatewayFailure(ctx context.Context, gateway *gatewayv1.Gateway, validationErr error) error {
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
	base := gateway.DeepCopy()
	gateway.Status.Addresses = nil
	setCondition(&gateway.Status.Conditions, condition(string(gatewayv1.GatewayConditionAccepted), metav1.ConditionFalse, string(gatewayReason), message, gateway.Generation))
	setCondition(&gateway.Status.Conditions, condition(string(gatewayv1.GatewayConditionProgrammed), metav1.ConditionFalse, string(gatewayv1.GatewayReasonInvalid), message, gateway.Generation))
	gateway.Status.Listeners = make([]gatewayv1.ListenerStatus, 0, len(gateway.Spec.Listeners))
	for _, listener := range gateway.Spec.Listeners {
		listenerStatus := gatewayv1.ListenerStatus{
			Name:       listener.Name,
			Conditions: existingListenerConditions(gateway, listener.Name),
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
	return r.patchGatewayStatus(ctx, gateway, base)
}

func listenerAllowsHTTPRoute(listener gatewayv1.Listener) bool {
	if listener.AllowedRoutes == nil || len(listener.AllowedRoutes.Kinds) == 0 {
		return true
	}
	for _, kind := range listener.AllowedRoutes.Kinds {
		if isHTTPRouteGroupKind(kind) {
			return true
		}
	}
	return false
}

func isHTTPRouteGroupKind(routeKind gatewayv1.RouteGroupKind) bool {
	group := gatewayv1.Group(gatewayv1.GroupName)
	if routeKind.Group != nil {
		group = *routeKind.Group
	}
	return group == gatewayv1.Group(gatewayv1.GroupName) && routeKind.Kind == gatewayv1.Kind("HTTPRoute")
}

func supportedHTTPRouteKinds() []gatewayv1.RouteGroupKind {
	group := gatewayv1.Group(gatewayv1.GroupName)
	return []gatewayv1.RouteGroupKind{{Group: &group, Kind: gatewayv1.Kind("HTTPRoute")}}
}

func existingListenerConditions(gateway *gatewayv1.Gateway, listenerName gatewayv1.SectionName) []metav1.Condition {
	for _, status := range gateway.Status.Listeners {
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

func (r *GatewayReconciler) setGatewayFailureReason(ctx context.Context, gateway *gatewayv1.Gateway, reason gatewayv1.GatewayConditionReason, message string) error {
	base := gateway.DeepCopy()
	gateway.Status.Addresses = nil
	gateway.Status.Listeners = listenerStatusesWithoutControllerConditions(gateway)
	setCondition(&gateway.Status.Conditions, condition(string(gatewayv1.GatewayConditionAccepted), metav1.ConditionFalse, string(reason), message, gateway.Generation))
	setCondition(&gateway.Status.Conditions, condition(string(gatewayv1.GatewayConditionProgrammed), metav1.ConditionFalse, string(gatewayv1.GatewayReasonInvalid), message, gateway.Generation))
	return r.patchGatewayStatus(ctx, gateway, base)
}

func (r *GatewayReconciler) setGatewayProgressing(ctx context.Context, gateway *gatewayv1.Gateway, listener *gatewayv1.Listener, message string) error {
	return r.setGatewayProvisioningStatus(ctx, gateway, listener, metav1.ConditionFalse, gatewayv1.GatewayReasonNoResources, gatewayv1.ListenerReasonPending, message, true)
}

func (r *GatewayReconciler) handleGatewayProviderFailure(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	listener *gatewayv1.Listener,
	providerErr error,
) (ctrl.Result, error) {
	policy, ok := providerFailurePolicyFor(providerErr)
	if !ok {
		return ctrl.Result{}, providerErr
	}
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
		return ctrl.Result{}, errors.Join(
			safeProviderReconcileError(policy, providerErr),
			fmt.Errorf("publish Gateway provider failure: %w", err),
		)
	}
	if transitioned {
		recordProviderWarning(r.Recorder, gateway, policy, "EnsureGateway")
	}
	return providerFailureResult(policy, providerErr, string(gateway.UID))
}

func (r *GatewayReconciler) handleGatewayDeleteFailure(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	providerErr error,
) (ctrl.Result, error) {
	policy, ok := providerFailurePolicyFor(providerErr)
	if !ok {
		return ctrl.Result{}, providerErr
	}
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
	if err := r.setGatewayCleanupFailure(ctx, gateway, policy, gatewayReason, listenerReason); err != nil {
		return ctrl.Result{}, errors.Join(
			safeProviderReconcileError(policy, providerErr),
			fmt.Errorf("publish Gateway cleanup failure: %w", err),
		)
	}
	if transitioned {
		recordProviderWarning(r.Recorder, gateway, policy, "DeleteGateway")
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
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	policy providerFailurePolicy,
	gatewayReason gatewayv1.GatewayConditionReason,
	listenerReason gatewayv1.ListenerConditionReason,
) error {
	base := gateway.DeepCopy()
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
	return r.patchGatewayStatus(ctx, gateway, base)
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
	base := gateway.DeepCopy()
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
	return r.patchGatewayStatus(ctx, gateway, base)
}

func (r *GatewayReconciler) setGatewayProgrammed(ctx context.Context, gateway *gatewayv1.Gateway, listener *gatewayv1.Listener, gatewayState cloud.GatewayState) error {
	attachedRoutes, err := r.attachedRouteCount(ctx, gateway, listener)
	if err != nil {
		return err
	}
	base := gateway.DeepCopy()
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
	return r.patchGatewayStatus(ctx, gateway, base)
}

func setAcceptedListenerConditions(conditions *[]metav1.Condition, generation int64) {
	setCondition(conditions, condition(string(gatewayv1.ListenerConditionAccepted), metav1.ConditionTrue, string(gatewayv1.ListenerReasonAccepted), "Listener is accepted", generation))
	setCondition(conditions, condition(string(gatewayv1.ListenerConditionResolvedRefs), metav1.ConditionTrue, string(gatewayv1.ListenerReasonResolvedRefs), "Listener references are resolved", generation))
	setCondition(conditions, condition(string(gatewayv1.ListenerConditionConflicted), metav1.ConditionFalse, string(gatewayv1.ListenerReasonNoConflicts), "Listener has no conflicts", generation))
}

func (r *GatewayReconciler) patchGatewayStatus(ctx context.Context, gateway, base *gatewayv1.Gateway) error {
	if equality.Semantic.DeepEqual(base.Status, gateway.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, gateway, optimisticMergeFrom(base)); err != nil {
		return fmt.Errorf("patch Gateway status: %w", err)
	}
	return nil
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

func resourceDisplayName(parts ...string) string {
	name := strings.Join(append([]string{openStackResourceNamePrefix}, parts...), "-")
	if len(name) > 200 {
		return name[:200]
	}
	return name
}

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
			builder.WithPredicates(updatePredicate(generationOrDeletionChanged)),
		).
		Named("gateway").
		Complete(r)
}
