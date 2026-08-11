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
	"net"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
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

type routeErrorKind string

var errHTTPRouteChanged = errors.New("HTTPRoute changed during reconciliation")

const (
	routeErrorUnsupported         routeErrorKind = "Unsupported"
	routeErrorInvalidKind         routeErrorKind = "InvalidKind"
	routeErrorRefNotPermitted     routeErrorKind = "RefNotPermitted"
	routeErrorUnsupportedProtocol routeErrorKind = "UnsupportedProtocol"
	routeErrorNoMatchingParent    routeErrorKind = "NoMatchingParent"
	routeErrorNotAllowed          routeErrorKind = "NotAllowedByListeners"
	routeErrorPending             routeErrorKind = "Pending"
)

type routeBuildError struct {
	kind    routeErrorKind
	message string
}

func (e *routeBuildError) Error() string { return e.message }

func newRouteBuildError(kind routeErrorKind, format string, args ...any) error {
	return &routeBuildError{kind: kind, message: fmt.Sprintf(format, args...)}
}

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

type routeGraphResult struct {
	ready           bool
	bindingRequired bool
}

type managedRouteParent struct {
	ref      gatewayv1.ParentReference
	gateway  *gatewayv1.Gateway
	listener *gatewayv1.Listener
}

// HTTPRouteReconciler owns the route-scoped Octavia resources for the Phase 1
// HTTP/NodePort profile.
type HTTPRouteReconciler struct {
	client.Client
	Provider    cloud.Provider
	Coordinator *GraphCoordinator
	APIReader   client.Reader
	Config      Config
}

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways;gatewayclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services;nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("httpRoute", req.NamespacedName)
	ctx = log.IntoContext(ctx, logger)

	httpRoute := &gatewayv1.HTTPRoute{}
	if err := r.Get(ctx, req.NamespacedName, httpRoute); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	logger.V(4).Info("Reconciling HTTPRoute", "generation", httpRoute.Generation)
	if !httpRoute.DeletionTimestamp.IsZero() {
		logger.V(1).Info("Finalizing HTTPRoute")
		return ctrl.Result{}, r.finalizeRoute(ctx, httpRoute)
	}

	parents, err := r.managedParents(ctx, httpRoute)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(parents) == 0 {
		return ctrl.Result{}, r.setRouteStatusesAndDetach(ctx, httpRoute, nil)
	}
	if len(httpRoute.Spec.ParentRefs) != 1 || len(parents) != 1 {
		status := rejectedRouteStatus(string(gatewayv1.RouteReasonUnsupportedValue), "Phase 1 supports exactly one Gateway parentRef")
		updates := make([]parentStatusUpdate, 0, len(parents))
		for _, parent := range parents {
			updates = append(updates, parentStatusUpdate{parent: parent.ref, status: status})
		}
		return ctrl.Result{}, r.setRouteStatusesAndDetach(ctx, httpRoute, updates)
	}

	parent := parents[0]
	if validationErr := validateRouteParent(httpRoute, parent); validationErr != nil {
		return ctrl.Result{}, r.setRouteStatusesAndDetach(ctx, httpRoute, []parentStatusUpdate{{parent: parent.ref, status: statusForRouteBuildError(validationErr)}})
	}
	if structurallySupportedRoute(httpRoute) {
		decision, err := r.evaluateRouteSlot(ctx, httpRoute)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !decision.canReserve {
			status := rejectedServiceProfileStatus(decision.rejection)
			return ctrl.Result{}, r.setRouteStatusesAndDetach(ctx, httpRoute, []parentStatusUpdate{{parent: parent.ref, status: status}})
		}
	}
	selected, err := r.isSelectedRoute(ctx, httpRoute, parent)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !selected {
		status := rejectedRouteStatus(string(gatewayv1.RouteReasonUnsupportedValue), "Phase 1 supports one HTTPRoute per Gateway; an older route is already selected")
		return ctrl.Result{}, r.setRouteStatusesAndDetach(ctx, httpRoute, []parentStatusUpdate{{parent: parent.ref, status: status}})
	}
	if err := r.detachPreviousGateway(ctx, httpRoute, parent.gateway); err != nil {
		statusErr := r.setRouteParentStatuses(ctx, httpRoute, []parentStatusUpdate{{parent: parent.ref, status: failedRouteStatus(err.Error())}})
		logger.Error(errors.Join(err, statusErr), "Could not detach HTTPRoute from previous Gateway")
		return ctrl.Result{}, errors.Join(err, statusErr)
	}

	graphResult, err := r.ensureRouteGraph(ctx, httpRoute, parent.gateway)
	if err != nil {
		var semanticError *routeBuildError
		if errors.As(err, &semanticError) || apierrors.IsNotFound(err) {
			status := statusForRouteBuildError(err)
			return ctrl.Result{}, r.setRouteStatusesAndDetach(ctx, httpRoute, []parentStatusUpdate{{parent: parent.ref, status: status}})
		}
		if errors.Is(err, errHTTPRouteChanged) {
			return ctrl.Result{}, err
		}
		statusErr := r.setRouteParentStatuses(ctx, httpRoute, []parentStatusUpdate{{parent: parent.ref, status: failedRouteStatus(err.Error())}})
		logger.Error(errors.Join(err, statusErr), "Could not reconcile OpenStack resources for HTTPRoute")
		if statusErr != nil {
			return ctrl.Result{}, errors.Join(err, statusErr)
		}
		return ctrl.Result{}, err
	}
	if graphResult.bindingRequired {
		if _, err := r.bindRoute(ctx, httpRoute, parent.gateway); err != nil {
			return ctrl.Result{}, fmt.Errorf("bind HTTPRoute to Gateway: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if !graphResult.ready {
		status := pendingRouteStatus("Controller-owned Octavia load balancer or listener is not ready")
		if err := r.setRouteParentStatuses(ctx, httpRoute, []parentStatusUpdate{{parent: parent.ref, status: status}}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if err := r.setRouteParentStatuses(ctx, httpRoute, []parentStatusUpdate{{parent: parent.ref, status: programmedRouteStatus()}}); err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("Programmed HTTPRoute", "gateway", client.ObjectKeyFromObject(parent.gateway))
	return ctrl.Result{}, nil
}

func (r *HTTPRouteReconciler) ensureRouteGraph(
	ctx context.Context,
	expectedRoute *gatewayv1.HTTPRoute,
	expectedGateway *gatewayv1.Gateway,
) (routeGraphResult, error) {
	release, err := acquireGatewayGraph(ctx, r.Coordinator, string(expectedGateway.UID))
	if err != nil {
		return routeGraphResult{}, fmt.Errorf("acquire Gateway graph: %w", err)
	}
	defer release()

	var route gatewayv1.HTTPRoute
	if err := r.Get(ctx, client.ObjectKeyFromObject(expectedRoute), &route); err != nil {
		return routeGraphResult{}, errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if !sameHTTPRouteRevision(&route, expectedRoute.UID, expectedRoute.Generation, false) {
		return routeGraphResult{}, errHTTPRouteChanged
	}

	var gateway gatewayv1.Gateway
	if err := r.Get(ctx, client.ObjectKeyFromObject(expectedGateway), &gateway); err != nil {
		return routeGraphResult{}, errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if gateway.UID != expectedGateway.UID || gateway.Generation != expectedGateway.Generation || !gateway.DeletionTimestamp.IsZero() {
		return routeGraphResult{}, errHTTPRouteChanged
	}
	var gatewayClass gatewayv1.GatewayClass
	if err := r.Get(ctx, types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)}, &gatewayClass); err != nil {
		return routeGraphResult{}, errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if gatewayClass.Spec.ControllerName != r.Config.ControllerName || gatewayClass.Spec.ParametersRef != nil || len(route.Spec.ParentRefs) != 1 {
		return routeGraphResult{}, errHTTPRouteChanged
	}
	if !controllerutil.ContainsFinalizer(&gateway, r.Config.gatewayFinalizer()) {
		return routeGraphResult{}, errHTTPRouteChanged
	}
	if err := validateGatewayBinding(r.Config, &gateway); err != nil {
		return routeGraphResult{}, errHTTPRouteChanged
	}

	listener := (*gatewayv1.Listener)(nil)
	if len(gateway.Spec.Listeners) == 1 {
		listener = &gateway.Spec.Listeners[0]
	}
	parent := managedRouteParent{ref: route.Spec.ParentRefs[0], gateway: &gateway, listener: listener}
	if err := validateRouteParent(&route, parent); err != nil {
		return routeGraphResult{}, err
	}
	if gateway.Annotations[r.Config.gatewayListenerPortAnnotation()] != strconv.Itoa(int(listener.Port)) {
		return routeGraphResult{}, errHTTPRouteChanged
	}
	if structurallySupportedRoute(&route) {
		decision, err := r.evaluateRouteSlot(ctx, &route)
		if err != nil {
			return routeGraphResult{}, err
		}
		if !decision.canReserve {
			return routeGraphResult{}, decision.rejection
		}
	}
	selected, err := r.isSelectedRoute(ctx, &route, parent)
	if err != nil {
		return routeGraphResult{}, err
	}
	if !selected {
		return routeGraphResult{}, newRouteBuildError(routeErrorUnsupported, "Phase 1 supports one HTTPRoute per Gateway; an older route is already selected")
	}
	spec, err := r.buildRouteSpec(ctx, &route, &gateway)
	if err != nil {
		return routeGraphResult{}, err
	}
	programmed := meta.FindStatusCondition(gateway.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	if programmed == nil || programmed.Status != metav1.ConditionTrue || programmed.ObservedGeneration != gateway.Generation {
		return routeGraphResult{}, nil
	}
	desiredBinding := route.DeepCopy()
	r.applyRouteBinding(desiredBinding, &gateway)
	if !routeBindingMetadataEqual(&route, desiredBinding) {
		return routeGraphResult{bindingRequired: true}, nil
	}

	gatewayState, found, err := r.Provider.GetGateway(ctx, spec.Identity)
	if err != nil {
		return routeGraphResult{}, fmt.Errorf("get OpenStack resources for Gateway: %w", err)
	}
	if !found {
		return routeGraphResult{}, nil
	}
	spec.Gateway = gatewayState
	if _, err := r.Provider.EnsureRoute(ctx, spec); err != nil {
		return routeGraphResult{}, fmt.Errorf("ensure OpenStack resources for HTTPRoute: %w", err)
	}
	return routeGraphResult{ready: true}, nil
}

func (r *HTTPRouteReconciler) setRouteStatusesAndDetach(ctx context.Context, route *gatewayv1.HTTPRoute, updates []parentStatusUpdate) error {
	statusErr := r.setRouteParentStatuses(ctx, route, updates)
	cleanupErr := r.detachRoute(ctx, route)
	return errors.Join(statusErr, cleanupErr)
}

func (r *HTTPRouteReconciler) detachPreviousGateway(ctx context.Context, route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) error {
	routeUID, generation, isDeleting := route.UID, route.Generation, !route.DeletionTimestamp.IsZero()
	current := &gatewayv1.HTTPRoute{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(route), current); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !sameHTTPRouteRevision(current, routeUID, generation, isDeleting) {
		return errHTTPRouteChanged
	}
	*route = *current

	stored, present, err := r.storedRouteIdentity(current)
	if err != nil {
		return err
	}
	if !present {
		if controllerutil.ContainsFinalizer(current, r.Config.routeFinalizer()) {
			return fmt.Errorf("HTTPRoute %s/%s has the controller finalizer but no complete stored Gateway identity", current.Namespace, current.Name)
		}
		return nil
	}
	if stored.GatewayNamespace == gateway.Namespace && stored.GatewayName == gateway.Name && stored.GatewayUID == string(gateway.UID) {
		return nil
	}
	return r.detachRoute(ctx, route)
}

func (r *HTTPRouteReconciler) managedParents(ctx context.Context, route *gatewayv1.HTTPRoute) ([]managedRouteParent, error) {
	parents := make([]managedRouteParent, 0, len(route.Spec.ParentRefs))
	for _, parentRef := range route.Spec.ParentRefs {
		if !isGatewayParentRef(parentRef) {
			continue
		}
		namespace := route.Namespace
		if parentRef.Namespace != nil {
			namespace = string(*parentRef.Namespace)
		}
		var gateway gatewayv1.Gateway
		if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: string(parentRef.Name)}, &gateway); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get parent Gateway: %w", err)
		}
		var class gatewayv1.GatewayClass
		if err := r.Get(ctx, types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)}, &class); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get parent GatewayClass: %w", err)
		}
		if class.Spec.ControllerName != r.Config.ControllerName {
			continue
		}
		var listener *gatewayv1.Listener
		if len(gateway.Spec.Listeners) == 1 {
			listener = &gateway.Spec.Listeners[0]
		}
		parents = append(parents, managedRouteParent{ref: parentRef, gateway: &gateway, listener: listener})
	}
	return parents, nil
}

func validateRouteParent(route *gatewayv1.HTTPRoute, parent managedRouteParent) error {
	if parent.gateway.DeletionTimestamp != nil {
		return newRouteBuildError(routeErrorPending, "parent Gateway is being deleted")
	}
	if parent.gateway.Namespace != route.Namespace {
		return newRouteBuildError(routeErrorNotAllowed, "cross-namespace parentRefs are not supported in Phase 1")
	}
	if parent.listener == nil {
		return newRouteBuildError(routeErrorUnsupported, "parent Gateway must have exactly one listener in Phase 1")
	}
	listener := parent.listener
	if listener.Protocol != gatewayv1.HTTPProtocolType || listener.TLS != nil {
		return newRouteBuildError(routeErrorUnsupported, "parent listener is not a supported plain HTTP listener")
	}
	if parent.ref.SectionName != nil && *parent.ref.SectionName != listener.Name {
		return newRouteBuildError(routeErrorNoMatchingParent, "parentRef sectionName %q does not select listener %q", *parent.ref.SectionName, listener.Name)
	}
	if parent.ref.Port != nil && *parent.ref.Port != listener.Port {
		return newRouteBuildError(routeErrorNoMatchingParent, "parentRef port %d does not select listener port %d", *parent.ref.Port, listener.Port)
	}
	if listener.AllowedRoutes != nil {
		allowsHTTPRoute := len(listener.AllowedRoutes.Kinds) == 0
		for _, kind := range listener.AllowedRoutes.Kinds {
			group := gatewayv1.Group(gatewayv1.GroupName)
			if kind.Group != nil {
				group = *kind.Group
			}
			if group == gatewayv1.Group(gatewayv1.GroupName) && kind.Kind == gatewayv1.Kind("HTTPRoute") {
				allowsHTTPRoute = true
			}
		}
		if !allowsHTTPRoute {
			return newRouteBuildError(routeErrorNotAllowed, "listener allowedRoutes does not include HTTPRoute")
		}
		if listener.AllowedRoutes.Namespaces != nil && listener.AllowedRoutes.Namespaces.From != nil &&
			*listener.AllowedRoutes.Namespaces.From != gatewayv1.NamespacesFromSame {
			return newRouteBuildError(routeErrorNotAllowed, "Phase 1 listeners only allow same-namespace HTTPRoutes")
		}
	}
	return nil
}

func (r *HTTPRouteReconciler) isSelectedRoute(ctx context.Context, route *gatewayv1.HTTPRoute, parent managedRouteParent) (bool, error) {
	var routes gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routes, client.MatchingFields{
		indexHTTPRouteByParentGateway: objectKeyString(client.ObjectKeyFromObject(parent.gateway)),
	}); err != nil {
		return false, fmt.Errorf("list HTTPRoutes for Phase 1 attachment selection: %w", err)
	}
	winner := route
	for index := range routes.Items {
		candidate := &routes.Items[index]
		if candidate.Name == route.Name || !candidate.DeletionTimestamp.IsZero() || len(candidate.Spec.ParentRefs) != 1 {
			continue
		}
		candidateParent := managedRouteParent{ref: candidate.Spec.ParentRefs[0], gateway: parent.gateway, listener: parent.listener}
		if !sameGatewayTarget(candidateParent.ref, candidate.Namespace, parent.ref, route.Namespace) ||
			validateRouteParent(candidate, candidateParent) != nil || !structurallySupportedRoute(candidate) {
			continue
		}
		canReserve, err := r.routeCanReserveSlot(ctx, candidate)
		if err != nil {
			return false, err
		}
		if !canReserve {
			continue
		}
		if routePrecedes(candidate, winner) {
			winner = candidate
		}
	}
	return winner.Name == route.Name, nil
}

type routeSlotDecision struct {
	canReserve bool
	rejection  error
}

// evaluateRouteSlot excludes a definitively rejected NodePort profile while
// allowing an otherwise valid route with temporarily unresolved references to
// retain deterministic selection. Callers must first verify that the route is
// structurally supported.
func (r *HTTPRouteReconciler) evaluateRouteSlot(ctx context.Context, route *gatewayv1.HTTPRoute) (routeSlotDecision, error) {
	backend := route.Spec.Rules[0].BackendRefs[0]
	var service corev1.Service
	if err := r.Get(ctx, types.NamespacedName{Namespace: route.Namespace, Name: string(backend.Name)}, &service); err != nil {
		if apierrors.IsNotFound(err) {
			return routeSlotDecision{canReserve: true}, nil
		}
		return routeSlotDecision{}, fmt.Errorf("get candidate backend Service: %w", err)
	}
	if service.Spec.Type != corev1.ServiceTypeNodePort {
		return routeSlotDecision{rejection: newRouteBuildError(routeErrorUnsupported, "backend Service %s must be type NodePort in Phase 1", service.Name)}, nil
	}
	if _, err := nodePortForBackend(&service, int32(*backend.Port)); err != nil {
		var buildError *routeBuildError
		if errors.As(err, &buildError) && buildError.kind == routeErrorPending {
			return routeSlotDecision{canReserve: true}, nil
		}
		return routeSlotDecision{rejection: err}, nil
	}
	return routeSlotDecision{canReserve: true}, nil
}

func (r *HTTPRouteReconciler) routeCanReserveSlot(ctx context.Context, route *gatewayv1.HTTPRoute) (bool, error) {
	decision, err := r.evaluateRouteSlot(ctx, route)
	return decision.canReserve, err
}

func routePrecedes(left, right *gatewayv1.HTTPRoute) bool {
	if !left.CreationTimestamp.Equal(&right.CreationTimestamp) {
		return left.CreationTimestamp.Before(&right.CreationTimestamp)
	}
	return left.Namespace+"/"+left.Name < right.Namespace+"/"+right.Name
}

func structurallySupportedRoute(route *gatewayv1.HTTPRoute) bool {
	if len(route.Spec.ParentRefs) != 1 || len(route.Spec.Hostnames) > 1 ||
		len(route.Spec.Hostnames) == 1 && strings.HasPrefix(string(route.Spec.Hostnames[0]), "*.") ||
		len(route.Spec.Rules) != 1 {
		return false
	}
	rule := route.Spec.Rules[0]
	if rule.Name != nil || len(rule.Filters) != 0 || rule.Timeouts != nil || rule.Retry != nil || rule.SessionPersistence != nil || len(rule.BackendRefs) != 1 {
		return false
	}
	if _, _, _, err := supportedMatch(rule.Matches); err != nil {
		return false
	}
	backend := rule.BackendRefs[0]
	backendNamespace := route.Namespace
	if backend.Namespace != nil {
		backendNamespace = string(*backend.Namespace)
	}
	return len(backend.Filters) == 0 && (backend.Weight == nil || *backend.Weight != 0) &&
		(backend.Group == nil || *backend.Group == gatewayv1.Group("")) &&
		(backend.Kind == nil || *backend.Kind == gatewayv1.Kind("Service")) &&
		backendNamespace == route.Namespace && backend.Port != nil
}

func (r *HTTPRouteReconciler) buildRouteSpec(ctx context.Context, route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) (cloud.RouteSpec, error) {
	if len(route.Spec.Hostnames) > 1 {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorUnsupported, "Phase 1 supports at most one exact hostname")
	}
	hostname := ""
	if len(route.Spec.Hostnames) == 1 {
		hostname = string(route.Spec.Hostnames[0])
		if strings.HasPrefix(hostname, "*.") {
			return cloud.RouteSpec{}, newRouteBuildError(routeErrorUnsupported, "wildcard hostnames are not supported in Phase 1")
		}
	}
	if len(route.Spec.Rules) != 1 {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorUnsupported, "Phase 1 supports exactly one HTTPRoute rule")
	}
	rule := route.Spec.Rules[0]
	if rule.Name != nil || len(rule.Filters) != 0 || rule.Timeouts != nil || rule.Retry != nil || rule.SessionPersistence != nil {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorUnsupported, "named rules, filters, timeouts, retries, and session persistence are not supported in Phase 1")
	}
	_, pathType, pathValue, err := supportedMatch(rule.Matches)
	if err != nil {
		return cloud.RouteSpec{}, err
	}
	if len(rule.BackendRefs) != 1 {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorUnsupported, "Phase 1 supports exactly one backendRef")
	}
	backend := rule.BackendRefs[0]
	if len(backend.Filters) != 0 {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorUnsupported, "backendRef filters are not supported in Phase 1")
	}
	if backend.Weight != nil && *backend.Weight == 0 {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorUnsupported, "a single Phase 1 backendRef must have non-zero weight")
	}
	if backend.Group != nil && *backend.Group != gatewayv1.Group("") {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorInvalidKind, "backendRef group %q is unsupported; use a core Service", *backend.Group)
	}
	if backend.Kind != nil && *backend.Kind != gatewayv1.Kind("Service") {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorInvalidKind, "backendRef kind %q is unsupported; use Service", *backend.Kind)
	}
	backendNamespace := route.Namespace
	if backend.Namespace != nil {
		backendNamespace = string(*backend.Namespace)
	}
	if backendNamespace != route.Namespace {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorRefNotPermitted, "cross-namespace backendRefs require ReferenceGrant and are deferred until Phase 3")
	}
	if backend.Port == nil {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorUnsupported, "backendRef port is required")
	}

	var service corev1.Service
	if err := r.Get(ctx, types.NamespacedName{Namespace: backendNamespace, Name: string(backend.Name)}, &service); err != nil {
		return cloud.RouteSpec{}, fmt.Errorf("get backend Service: %w", err)
	}
	if service.Spec.Type != corev1.ServiceTypeNodePort {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorUnsupported, "backend Service %s must be type NodePort in Phase 1", service.Name)
	}
	servicePort, err := nodePortForBackend(&service, int32(*backend.Port))
	if err != nil {
		return cloud.RouteSpec{}, err
	}
	members, err := r.nodePortMembers(ctx, &service, servicePort.NodePort)
	if err != nil {
		return cloud.RouteSpec{}, err
	}
	return cloud.RouteSpec{
		Identity:       routeIdentity(r.Config, gateway, route),
		MemberSubnetID: r.Config.MemberSubnetID,
		HealthPath:     r.Config.HealthPath,
		Hostname:       hostname,
		PathType:       pathType,
		PathValue:      pathValue,
		Members:        members,
	}, nil
}

func supportedMatch(matches []gatewayv1.HTTPRouteMatch) (*gatewayv1.HTTPRouteMatch, cloud.PathMatchType, string, error) {
	if len(matches) == 0 {
		return nil, cloud.PathMatchPrefix, "/", nil
	}
	if len(matches) != 1 {
		return nil, "", "", newRouteBuildError(routeErrorUnsupported, "Phase 1 supports at most one HTTPRoute match")
	}
	match := &matches[0]
	if len(match.Headers) != 0 || len(match.QueryParams) != 0 || match.Method != nil {
		return nil, "", "", newRouteBuildError(routeErrorUnsupported, "header, query parameter, and method matches are not supported in Phase 1")
	}
	pathType := gatewayv1.PathMatchPathPrefix
	pathValue := "/"
	if match.Path != nil {
		if match.Path.Type != nil {
			pathType = *match.Path.Type
		}
		if match.Path.Value != nil {
			pathValue = *match.Path.Value
		}
	}
	if !validHTTPPath(pathValue) {
		return nil, "", "", newRouteBuildError(routeErrorUnsupported, "path %q is not a valid absolute Gateway API path", pathValue)
	}
	switch pathType {
	case gatewayv1.PathMatchExact:
		return match, cloud.PathMatchExact, pathValue, nil
	case gatewayv1.PathMatchPathPrefix:
		return match, cloud.PathMatchPrefix, pathValue, nil
	default:
		return nil, "", "", newRouteBuildError(routeErrorUnsupported, "path match type %q is not supported", pathType)
	}
}

func validHTTPPath(value string) bool {
	if !strings.HasPrefix(value, "/") || strings.Contains(value, "//") || strings.Contains(value, "/./") || strings.Contains(value, "/../") {
		return false
	}
	return value != "/." && value != "/.." && !strings.HasSuffix(value, "/.") && !strings.HasSuffix(value, "/..")
}

func nodePortForBackend(service *corev1.Service, port int32) (*corev1.ServicePort, error) {
	for index := range service.Spec.Ports {
		candidate := &service.Spec.Ports[index]
		if candidate.Port != port {
			continue
		}
		if candidate.Protocol != "" && candidate.Protocol != corev1.ProtocolTCP {
			return nil, newRouteBuildError(routeErrorUnsupportedProtocol, "backend Service port %d must use TCP", port)
		}
		if candidate.AppProtocol != nil && *candidate.AppProtocol != "http" && *candidate.AppProtocol != "kubernetes.io/http" {
			return nil, newRouteBuildError(routeErrorUnsupportedProtocol, "backend Service appProtocol %q is unsupported", *candidate.AppProtocol)
		}
		if candidate.NodePort == 0 {
			return nil, newRouteBuildError(routeErrorUnsupported, "backend Service port %d has no allocated NodePort", port)
		}
		return candidate, nil
	}
	return nil, newRouteBuildError(routeErrorPending, "backend Service %s does not expose port %d", service.Name, port)
}

func (r *HTTPRouteReconciler) nodePortMembers(ctx context.Context, service *corev1.Service, nodePort int32) ([]cloud.Member, error) {
	var slices discoveryv1.EndpointSliceList
	if err := r.List(ctx, &slices, client.MatchingFields{
		indexEndpointSliceByService: objectKeyString(client.ObjectKeyFromObject(service)),
	}); err != nil {
		return nil, fmt.Errorf("list backend EndpointSlices: %w", err)
	}
	readyEndpoint := false
	localNodes := map[string]struct{}{}
	for _, slice := range slices.Items {
		for _, endpoint := range slice.Endpoints {
			if !endpointReady(endpoint.Conditions) {
				continue
			}
			readyEndpoint = true
			if endpoint.NodeName != nil {
				localNodes[*endpoint.NodeName] = struct{}{}
			}
		}
	}
	if !readyEndpoint {
		return nil, newRouteBuildError(routeErrorPending, "backend Service %s has no ready endpoints", service.Name)
	}
	if service.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal && len(localNodes) == 0 {
		return nil, newRouteBuildError(routeErrorPending, "backend Service %s uses externalTrafficPolicy Local but ready endpoints do not identify their Nodes", service.Name)
	}

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("list backend Nodes: %w", err)
	}
	members := make([]cloud.Member, 0, len(nodes.Items))
	seen := map[string]struct{}{}
	for index := range nodes.Items {
		node := &nodes.Items[index]
		if !readyNode(node) {
			continue
		}
		if service.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal {
			if _, ok := localNodes[node.Name]; !ok {
				continue
			}
		}
		address := nodeAddress(node, r.Config.NodeAddressType)
		if address == "" {
			continue
		}
		key := net.JoinHostPort(address, fmt.Sprintf("%d", nodePort))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		members = append(members, cloud.Member{Address: address, Port: int(nodePort)})
	}
	if len(members) == 0 {
		return nil, newRouteBuildError(routeErrorPending, "backend Service %s has no ready Nodes with a %s address", service.Name, r.Config.NodeAddressType)
	}
	sort.Slice(members, func(i, j int) bool {
		if members[i].Address == members[j].Address {
			return members[i].Port < members[j].Port
		}
		return members[i].Address < members[j].Address
	})
	return members, nil
}

func endpointReady(conditions discoveryv1.EndpointConditions) bool {
	if conditions.Terminating != nil && *conditions.Terminating {
		return false
	}
	return conditions.Ready == nil || *conditions.Ready
}

func readyNode(node *corev1.Node) bool {
	if !node.DeletionTimestamp.IsZero() || node.Spec.Unschedulable {
		return false
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func nodeAddress(node *corev1.Node, addressType corev1.NodeAddressType) string {
	for _, address := range node.Status.Addresses {
		if address.Type == addressType && net.ParseIP(address.Address) != nil {
			return address.Address
		}
	}
	return ""
}

func (r *HTTPRouteReconciler) bindRoute(ctx context.Context, route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) (bool, error) {
	desired := routeIdentity(r.Config, gateway, route)
	routeKey := client.ObjectKeyFromObject(route)
	routeUID, generation, isDeleting := route.UID, route.Generation, !route.DeletionTimestamp.IsZero()
	bindingStored := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &gatewayv1.HTTPRoute{}
		if err := r.Get(ctx, routeKey, current); err != nil {
			return err
		}
		if !sameHTTPRouteRevision(current, routeUID, generation, isDeleting) {
			return errHTTPRouteChanged
		}

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

		base := current.DeepCopy()
		r.applyRouteBinding(current, gateway)
		if routeBindingMetadataEqual(base, current) {
			*route = *current
			return nil
		}
		if err := r.Patch(ctx, current, optimisticMergeFrom(base)); err != nil {
			return err
		}
		bindingStored = true
		*route = *current
		log.FromContext(ctx).V(1).Info("Bound HTTPRoute to Gateway", "gateway", client.ObjectKeyFromObject(gateway))
		return nil
	})
	return bindingStored, err
}

func (r *HTTPRouteReconciler) detachRoute(ctx context.Context, route *gatewayv1.HTTPRoute) error {
	routeKey := client.ObjectKeyFromObject(route)
	routeUID, generation, isDeleting := route.UID, route.Generation, !route.DeletionTimestamp.IsZero()
	current, stored, present, err := r.getRouteBinding(ctx, routeKey, routeUID, generation, isDeleting)
	if err != nil {
		return err
	}
	if current == nil {
		return nil
	}
	if !present && controllerutil.ContainsFinalizer(current, r.Config.routeFinalizer()) {
		return fmt.Errorf("HTTPRoute %s has the controller finalizer but no complete stored Gateway identity", routeKey)
	}
	if present {
		if err := r.deleteRoute(ctx, current, stored); err != nil {
			return fmt.Errorf("delete detached HTTPRoute resources: %w", err)
		}
		log.FromContext(ctx).V(1).Info("Deleted detached HTTPRoute resources", "gateway", types.NamespacedName{Namespace: stored.GatewayNamespace, Name: stored.GatewayName})
	}
	if err := r.clearStoredRouteBinding(ctx, route, stored, present); err != nil {
		return err
	}
	log.FromContext(ctx).V(1).Info("Detached HTTPRoute from Gateway")
	return nil
}

func (r *HTTPRouteReconciler) finalizeRoute(ctx context.Context, route *gatewayv1.HTTPRoute) error {
	routeKey := client.ObjectKeyFromObject(route)
	current, stored, present, err := r.getRouteBinding(
		ctx,
		routeKey,
		route.UID,
		route.Generation,
		!route.DeletionTimestamp.IsZero(),
	)
	if err != nil {
		return err
	}
	if current == nil {
		return nil
	}
	if !controllerutil.ContainsFinalizer(current, r.Config.routeFinalizer()) {
		return nil
	}
	if !present {
		return fmt.Errorf("HTTPRoute %s has the controller finalizer but no complete stored Gateway identity", routeKey)
	}
	if err := r.deleteRoute(ctx, current, stored); err != nil {
		return fmt.Errorf("delete HTTPRoute resources during finalization: %w", err)
	}
	log.FromContext(ctx).V(1).Info("Deleted HTTPRoute resources during finalization", "gateway", types.NamespacedName{Namespace: stored.GatewayNamespace, Name: stored.GatewayName})
	if err := r.clearStoredRouteBinding(ctx, route, stored, true); err != nil {
		return err
	}
	log.FromContext(ctx).V(1).Info("Removed HTTPRoute finalizer")
	return nil
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

func (r *HTTPRouteReconciler) deleteRoute(ctx context.Context, expected *gatewayv1.HTTPRoute, identity cloud.Identity) error {
	if r.APIReader == nil {
		return errAPIReaderRequired
	}
	release, err := acquireGatewayGraph(ctx, r.Coordinator, identity.GatewayUID)
	if err != nil {
		return fmt.Errorf("acquire Gateway graph: %w", err)
	}
	defer release()

	var current gatewayv1.HTTPRoute
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(expected), &current); err != nil {
		return errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if !sameHTTPRouteRevision(&current, expected.UID, expected.Generation, !expected.DeletionTimestamp.IsZero()) ||
		current.ResourceVersion != expected.ResourceVersion {
		return errHTTPRouteChanged
	}
	stored, present, err := r.storedRouteIdentity(&current)
	if err != nil {
		return err
	}
	if !present || stored != identity {
		return errHTTPRouteChanged
	}

	return r.Provider.DeleteRoute(ctx, stored)
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
	annotations := route.Annotations
	values := []string{
		annotations[r.Config.routeGatewayNamespaceAnnotation()],
		annotations[r.Config.routeGatewayNameAnnotation()],
		annotations[r.Config.routeGatewayUIDAnnotation()],
	}
	empty := 0
	for _, value := range values {
		if value == "" {
			empty++
		}
	}
	if empty == len(values) {
		if annotations[r.Config.routeClusterIDAnnotation()] != "" || annotations[r.Config.routeProjectIDAnnotation()] != "" || r.hasRouteBindingFinalizer(route) {
			return cloud.Identity{}, false, fmt.Errorf("HTTPRoute %s/%s has an incomplete stored Gateway identity", route.Namespace, route.Name)
		}
		return cloud.Identity{}, false, nil
	}
	if empty != 0 {
		return cloud.Identity{}, false, fmt.Errorf("HTTPRoute %s/%s has an incomplete stored Gateway identity", route.Namespace, route.Name)
	}
	clusterID := annotations[r.Config.routeClusterIDAnnotation()]
	if clusterID == "" {
		clusterID = r.Config.ClusterID
	}
	projectID := annotations[r.Config.routeProjectIDAnnotation()]
	if projectID == "" {
		projectID = r.Config.OpenStackProjectID
	}
	identity := cloud.Identity{
		OpenStackProjectID: projectID,
		ClusterID:          clusterID,
		Controller:         string(r.Config.ControllerName),
		ControllerVersion:  r.Config.ControllerVersion,
		GatewayNamespace:   values[0],
		GatewayName:        values[1],
		GatewayUID:         values[2],
		RouteNamespace:     route.Namespace,
		RouteName:          route.Name,
		RouteUID:           string(route.UID),
	}
	if identity.ClusterID != r.Config.ClusterID {
		return cloud.Identity{}, false, fmt.Errorf("controller cluster identity differs from HTTPRoute %s/%s's existing resource binding; restore the original cluster ID before reconciling", route.Namespace, route.Name)
	}
	if identity.OpenStackProjectID != r.Config.OpenStackProjectID {
		return cloud.Identity{}, false, fmt.Errorf("authenticated OpenStack project differs from HTTPRoute %s/%s's existing resource binding; restore access to the original project before reconciling", route.Namespace, route.Name)
	}
	if err := cloud.ValidateRouteIdentity(identity); err != nil {
		return cloud.Identity{}, false, err
	}
	expectedBindingFinalizer := r.Config.routeBindingFinalizer(
		identity.ClusterID,
		identity.OpenStackProjectID,
		identity.GatewayNamespace,
		identity.GatewayName,
		identity.GatewayUID,
	)
	if controllerutil.ContainsFinalizer(route, r.Config.routeFinalizer()) && !controllerutil.ContainsFinalizer(route, expectedBindingFinalizer) {
		return cloud.Identity{}, false, fmt.Errorf("HTTPRoute %s/%s stored Gateway identity does not match its binding finalizer; restore the original binding before reconciling", route.Namespace, route.Name)
	}
	for _, finalizer := range route.Finalizers {
		if strings.HasPrefix(finalizer, r.Config.routeBindingFinalizerPrefix()) && finalizer != expectedBindingFinalizer {
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

func (r *HTTPRouteReconciler) hasRouteBindingFinalizer(route *gatewayv1.HTTPRoute) bool {
	for _, finalizer := range route.Finalizers {
		if strings.HasPrefix(finalizer, r.Config.routeBindingFinalizerPrefix()) {
			return true
		}
	}
	return false
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

func failedRouteStatus(message string) routeReconcileStatus {
	status := programmedRouteStatus()
	status.programmed = metav1.ConditionFalse
	status.programmedReason = "OpenStackError"
	status.programmedMessage = message
	return status
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
	routeKey := client.ObjectKeyFromObject(route)
	observedGeneration := route.Generation
	routeUID := route.UID
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &gatewayv1.HTTPRoute{}
		if err := r.Get(ctx, routeKey, current); err != nil {
			return client.IgnoreNotFound(err)
		}
		// A newer reconciliation owns status for a changed spec. Avoid publishing
		// conditions that were calculated from the previous generation.
		if current.UID != routeUID || current.Generation != observedGeneration || !current.DeletionTimestamp.IsZero() {
			return nil
		}

		base := current.DeepCopy()
		existing := append([]gatewayv1.RouteParentStatus(nil), current.Status.Parents...)
		parents := make([]gatewayv1.RouteParentStatus, 0, len(existing)+len(updates))
		for _, parent := range existing {
			if parent.ControllerName != r.Config.ControllerName {
				parents = append(parents, parent)
			}
		}
		for _, update := range updates {
			parentStatus := gatewayv1.RouteParentStatus{ParentRef: update.parent, ControllerName: r.Config.ControllerName}
			for _, candidate := range existing {
				if candidate.ControllerName == r.Config.ControllerName && parentRefsEqual(candidate.ParentRef, current.Namespace, update.parent, current.Namespace) {
					parentStatus.Conditions = append([]metav1.Condition(nil), candidate.Conditions...)
					break
				}
			}
			setCondition(&parentStatus.Conditions, condition(string(gatewayv1.RouteConditionAccepted), update.status.accepted, update.status.acceptedReason, update.status.acceptedMessage, observedGeneration))
			setCondition(&parentStatus.Conditions, condition(string(gatewayv1.RouteConditionResolvedRefs), update.status.resolved, update.status.resolvedReason, update.status.resolvedMessage, observedGeneration))
			setCondition(&parentStatus.Conditions, condition(r.Config.domain()+"/Programmed", update.status.programmed, update.status.programmedReason, update.status.programmedMessage, observedGeneration))
			parents = append(parents, parentStatus)
		}
		current.Status.Parents = parents
		if reflect.DeepEqual(base.Status, current.Status) {
			*route = *current
			return nil
		}
		if err := r.Status().Patch(ctx, current, optimisticMergeFrom(base)); err != nil {
			return err
		}
		*route = *current
		log.FromContext(ctx).V(1).Info("Updated HTTPRoute status")
		return nil
	})
}

func parentRefsEqual(left gatewayv1.ParentReference, leftRouteNamespace string, right gatewayv1.ParentReference, rightRouteNamespace string) bool {
	leftGroup, rightGroup := gatewayv1.Group(gatewayv1.GroupName), gatewayv1.Group(gatewayv1.GroupName)
	if left.Group != nil {
		leftGroup = *left.Group
	}
	if right.Group != nil {
		rightGroup = *right.Group
	}
	leftKind, rightKind := gatewayv1.Kind("Gateway"), gatewayv1.Kind("Gateway")
	if left.Kind != nil {
		leftKind = *left.Kind
	}
	if right.Kind != nil {
		rightKind = *right.Kind
	}
	leftNamespace, rightNamespace := leftRouteNamespace, rightRouteNamespace
	if left.Namespace != nil {
		leftNamespace = string(*left.Namespace)
	}
	if right.Namespace != nil {
		rightNamespace = string(*right.Namespace)
	}
	return leftGroup == rightGroup && leftKind == rightKind && leftNamespace == rightNamespace && left.Name == right.Name &&
		stringPointerEqual(left.SectionName, right.SectionName) && int32PointerEqual(left.Port, right.Port)
}

func sameGatewayTarget(left gatewayv1.ParentReference, leftRouteNamespace string, right gatewayv1.ParentReference, rightRouteNamespace string) bool {
	leftCopy, rightCopy := left, right
	leftCopy.SectionName, leftCopy.Port = nil, nil
	rightCopy.SectionName, rightCopy.Port = nil, nil
	return parentRefsEqual(leftCopy, leftRouteNamespace, rightCopy, rightRouteNamespace)
}

func stringPointerEqual[T ~string](left, right *T) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func int32PointerEqual(left, right *gatewayv1.PortNumber) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

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
