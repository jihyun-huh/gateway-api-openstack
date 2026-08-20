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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller/graph"
)

var errHTTPRouteChanged = errors.New("HTTPRoute changed during reconciliation")

type cachedRouteDisposition int

const (
	cachedRouteUnknown cachedRouteDisposition = iota
	cachedRouteUnsupportedVersion
	cachedRouteDetach
	cachedRouteReady
)

// cachedRouteDecision records the Kubernetes cache view used for status and
// lifecycle routing. Provider mutations build and seal a separate live
// routeValidationSnapshot while holding the Gateway graph lock.
type cachedRouteDecision struct {
	parent         managedRouteParent
	storedIdentity cloud.Identity
	bindingPresent bool
	statusUpdates  []parentStatusUpdate
	disposition    cachedRouteDisposition
}

// Reconciler owns the route-scoped Octavia resources for the Phase 1
// HTTP/NodePort profile.
type Reconciler struct {
	// Client reads and patches cached Kubernetes objects.
	client.Client
	// Provider reconciles the provider-neutral OpenStack resource graph.
	Provider cloud.Provider
	// Coordinator serializes graph mutation by Gateway UID.
	Coordinator *graph.Coordinator
	// APIReader performs safety-sensitive live reads.
	APIReader client.Reader
	// Recorder publishes Kubernetes Events.
	Recorder events.EventRecorder
	// Config contains immutable controller configuration.
	Config controller.Config
}

// Reconcile evaluates one HTTPRoute and converges the route resources in its
// bound Gateway graph.
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways;gatewayclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services;nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch;update
func (r *Reconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (result ctrl.Result, retErr error) {
	logger := log.FromContext(ctx).WithValues("httpRoute", req.NamespacedName)
	ctx = log.IntoContext(ctx, logger)

	scope, responsible, err := r.newHTTPRouteScope(ctx, req)
	if err != nil || !responsible {
		return ctrl.Result{}, err
	}
	defer func() {
		if err := r.patchHTTPRoute(ctx, scope); err != nil {
			result = ctrl.Result{}
			retErr = errors.Join(retErr, scope.patchCause, err)
		}
	}()

	logger.V(4).Info("Reconciling HTTPRoute", "generation", scope.route.Generation)
	if !scope.route.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, scope)
	}
	return r.reconcileNormal(ctx, scope)
}

func (r *Reconciler) reconcileDelete(ctx context.Context, scope *httpRouteScope) (ctrl.Result, error) {
	log.FromContext(ctx).V(1).Info("Finalizing HTTPRoute")
	result, err := r.finalizeRoute(ctx, scope)
	if err == nil && result.RequeueAfter == 0 && !controllerutil.ContainsFinalizer(scope.route, r.Config.RouteFinalizer()) {
		scope.skipPatch = true
	}
	return result, err
}

func (r *Reconciler) reconcileNormal(ctx context.Context, scope *httpRouteScope) (ctrl.Result, error) {
	decision, err := r.buildCachedRouteDecision(ctx, scope.route)
	if err != nil {
		return ctrl.Result{}, err
	}

	switch decision.disposition {
	case cachedRouteUnsupportedVersion:
		return r.setRouteUnsupportedVersion(ctx, scope)
	case cachedRouteDetach:
		return r.setRouteStatusesAndDetach(ctx, scope, decision.statusUpdates)
	case cachedRouteReady:
		return r.reconcileSelectedRoute(ctx, scope, decision)
	default:
		return ctrl.Result{}, fmt.Errorf("invalid cached HTTPRoute decision disposition %d", decision.disposition)
	}
}

func (r *Reconciler) buildCachedRouteDecision(
	ctx context.Context,
	route *gatewayv1.HTTPRoute,
) (cachedRouteDecision, error) {
	parents, err := r.managedParents(ctx, route)
	if err != nil {
		if _, _, bindingErr := r.storedRouteIdentity(route); bindingErr != nil {
			return cachedRouteDecision{}, bindingErr
		}
		return cachedRouteDecision{}, err
	}
	storedIdentity, bindingPresent, err := r.storedRouteIdentity(route)
	if err != nil {
		return cachedRouteDecision{}, err
	}
	base := cachedRouteDecision{
		storedIdentity: storedIdentity,
		bindingPresent: bindingPresent,
	}
	if !managedParentsSupportInstalledVersion(parents) {
		base.disposition = cachedRouteUnsupportedVersion
		return base, nil
	}
	if len(parents) == 0 {
		base.disposition = cachedRouteDetach
		return base, nil
	}
	if len(route.Spec.ParentRefs) != 1 || len(parents) != 1 {
		base.disposition = cachedRouteDetach
		base.statusUpdates = unsupportedParentCountUpdates(parents)
		return base, nil
	}
	return r.validateCachedRouteDecision(ctx, route, parents[0], base)
}

func (r *Reconciler) validateCachedRouteDecision(
	ctx context.Context,
	route *gatewayv1.HTTPRoute,
	parent managedRouteParent,
	decision cachedRouteDecision,
) (cachedRouteDecision, error) {
	decision.parent = parent
	if validationErr := validateRouteParent(route, parent); validationErr != nil {
		decision.disposition = cachedRouteDetach
		decision.statusUpdates = []parentStatusUpdate{{
			parent: parent.ref,
			status: statusForRouteBuildError(validationErr),
		}}
		return decision, nil
	}
	if structurallySupportedRoute(route) {
		slot, err := r.evaluateRouteSlot(ctx, route)
		if err != nil {
			return cachedRouteDecision{}, err
		}
		if !slot.canReserve {
			decision.disposition = cachedRouteDetach
			decision.statusUpdates = []parentStatusUpdate{{
				parent: parent.ref,
				status: rejectedServiceProfileStatus(slot.rejection),
			}}
			return decision, nil
		}
	}
	selected, err := r.isSelectedRoute(ctx, route, parent)
	if err != nil {
		return cachedRouteDecision{}, err
	}
	if !selected {
		decision.disposition = cachedRouteDetach
		decision.statusUpdates = []parentStatusUpdate{{
			parent: parent.ref,
			status: rejectedRouteStatus(
				string(gatewayv1.RouteReasonUnsupportedValue),
				"Phase 1 supports one HTTPRoute per Gateway; an older route is already selected",
			),
		}}
		return decision, nil
	}
	decision.disposition = cachedRouteReady
	return decision, nil
}

func managedParentsSupportInstalledVersion(parents []managedRouteParent) bool {
	for _, parent := range parents {
		if !controller.GatewayClassSupportsInstalledVersion(parent.gatewayClass) {
			return false
		}
	}
	return true
}

func unsupportedParentCountUpdates(parents []managedRouteParent) []parentStatusUpdate {
	status := rejectedRouteStatus(
		string(gatewayv1.RouteReasonUnsupportedValue),
		"Phase 1 supports exactly one Gateway parentRef",
	)
	updates := make([]parentStatusUpdate, 0, len(parents))
	for _, parent := range parents {
		updates = append(updates, parentStatusUpdate{parent: parent.ref, status: status})
	}
	return updates
}

func (r *Reconciler) reconcileSelectedRoute(
	ctx context.Context,
	scope *httpRouteScope,
	decision cachedRouteDecision,
) (ctrl.Result, error) {
	httpRoute := scope.route
	parent := decision.parent
	if decision.bindingPresent && !controller.SameGatewayIdentity(
		decision.storedIdentity,
		controller.RouteIdentity(r.Config, parent.gateway, httpRoute),
	) {
		updates := []parentStatusUpdate{{
			parent: parent.ref,
			status: pendingRouteStatus("OpenStack resources for the previous Gateway are being removed"),
		}}
		checkpointNeeded, err := r.routeCleanupStatusCheckpointNeeded(ctx, scope, updates)
		if err != nil {
			return ctrl.Result{}, err
		}
		if checkpointNeeded {
			return ctrl.Result{Requeue: true}, nil
		}
	}
	detachResult, err := r.detachPreviousGateway(ctx, httpRoute, parent.gateway)
	if err != nil {
		if errors.Is(err, controller.ErrUnsupportedGatewayAPIVersion) {
			return r.setRouteUnsupportedVersion(ctx, scope)
		}
		return r.handleRouteProviderFailure(scope, parent.ref, false, err, "DeleteHTTPRoute")
	}
	if detachResult.RequeueAfter > 0 {
		status := pendingRouteStatus("OpenStack resources for the previous Gateway are being removed")
		scope.setStatuses([]parentStatusUpdate{{parent: parent.ref, status: status}})
		return detachResult, nil
	}
	return r.reconcileSelectedRouteGraph(ctx, scope, parent)
}

func (r *Reconciler) reconcileSelectedRouteGraph(
	ctx context.Context,
	scope *httpRouteScope,
	parent managedRouteParent,
) (ctrl.Result, error) {
	httpRoute := scope.route
	graphResult, err := r.ensureRouteGraph(ctx, httpRoute, parent.gateway)
	if err != nil {
		if errors.Is(err, controller.ErrUnsupportedGatewayAPIVersion) {
			return r.setRouteUnsupportedVersion(ctx, scope)
		}
		var bindingError *routeBindingCheckpointError
		if errors.As(err, &bindingError) {
			return ctrl.Result{}, fmt.Errorf("bind HTTPRoute to Gateway: %w", bindingError)
		}
		var semanticError *routeBuildError
		if errors.As(err, &semanticError) || apierrors.IsNotFound(err) {
			status := statusForRouteBuildError(err)
			return r.setRouteStatusesAndDetach(ctx, scope, []parentStatusUpdate{{parent: parent.ref, status: status}})
		}
		if errors.Is(err, errHTTPRouteChanged) {
			return ctrl.Result{}, err
		}
		return r.handleRouteProviderFailure(scope, parent.ref, true, err, "EnsureHTTPRoute")
	}
	if graphResult.bindingCheckpoint {
		return ctrl.Result{Requeue: true}, nil
	}
	if graphResult.outcome.State == cloud.OutcomeProgressing {
		message := controller.ProviderProgressMessage(graphResult.outcome, "Octavia resources for the HTTPRoute are still progressing")
		status := pendingRouteStatus(message)
		scope.setStatuses([]parentStatusUpdate{{parent: parent.ref, status: status}})
		return ctrl.Result{RequeueAfter: controller.ProviderProgressRequeueAfter(graphResult.outcome, httpRoute.UID)}, nil
	}
	if graphResult.outcome.State != cloud.OutcomeReady {
		status := pendingRouteStatus("Controller-owned Octavia load balancer or listener is not ready")
		scope.setStatuses([]parentStatusUpdate{{parent: parent.ref, status: status}})
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	scope.setStatuses([]parentStatusUpdate{{parent: parent.ref, status: programmedRouteStatus()}})
	log.FromContext(ctx).V(1).Info("Programmed HTTPRoute", "gateway", client.ObjectKeyFromObject(parent.gateway))
	return ctrl.Result{RequeueAfter: controller.OpenStackResyncAfter(r.Config.OpenStackResyncInterval, httpRoute.UID)}, nil
}
