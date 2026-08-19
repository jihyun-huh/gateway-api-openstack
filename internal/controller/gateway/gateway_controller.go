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

package gateway

import (
	"context"
	"errors"
	"strconv"

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

// Reconciler owns the Gateway-scoped Octavia resources.
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

var (
	errGatewayChanged = errors.New("gateway changed during reconciliation")
)

// Reconcile evaluates one Gateway and converges the graph owned by it.
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses;httproutes,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch;update
func (r *Reconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (result ctrl.Result, retErr error) {
	logger := log.FromContext(ctx).WithValues("gateway", req.NamespacedName)
	ctx = log.IntoContext(ctx, logger)
	logger.V(4).Info("Reconciling Gateway")

	scope, responsible, err := r.newGatewayScope(ctx, req)
	if err != nil || !responsible {
		return ctrl.Result{}, err
	}
	defer func() {
		if err := r.patchGateway(ctx, scope); err != nil {
			result = ctrl.Result{}
			retErr = errors.Join(retErr, scope.patchCause, err)
		}
	}()

	if !scope.gateway.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, scope)
	}
	return r.reconcileNormal(ctx, scope)
}

func (r *Reconciler) reconcileDelete(ctx context.Context, scope *gatewayScope) (ctrl.Result, error) {
	result, err := r.cleanupGateway(ctx, scope)
	if err != nil || result.RequeueAfter > 0 {
		return result, err
	}
	log.FromContext(ctx).V(1).Info("Cleaned up OpenStack resources for Gateway")
	return ctrl.Result{}, nil
}

func (r *Reconciler) reconcileNormal(ctx context.Context, scope *gatewayScope) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	gateway := scope.gateway

	if !scope.managed {
		if controller.GatewayHasControllerBinding(r.Config, gateway) {
			result, err := r.cleanupGateway(ctx, scope)
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
	if scope.gatewayClass != nil && !controller.GatewayClassSupportsInstalledVersion(scope.gatewayClass) {
		return r.setGatewayUnsupportedVersion(ctx, scope)
	}
	if err := controller.ValidateGatewayBinding(r.Config, gateway); err != nil {
		r.setGatewayFailureReason(gateway, gatewayv1.GatewayReasonInvalidParameters, err.Error())
		return ctrl.Result{}, nil
	}
	if scope.gatewayClass != nil && scope.gatewayClass.Spec.ParametersRef != nil {
		r.setGatewayFailureReason(gateway, gatewayv1.GatewayReasonInvalidParameters, "GatewayClass parametersRef is not supported in Phase 1")
		if scope.statusChanged() {
			return ctrl.Result{Requeue: true}, nil
		}
		return r.cleanupGateway(ctx, scope)
	}

	gatewayListener, validationErr := Validate(gateway)
	if validationErr != nil {
		r.setGatewayFailure(gateway, validationErr)
		if scope.statusChanged() {
			return ctrl.Result{Requeue: true}, nil
		}
		return r.cleanupGateway(ctx, scope)
	}
	desiredListenerPort := strconv.Itoa(int(gatewayListener.Port))
	storedListenerPort := gateway.Annotations[r.Config.GatewayListenerPortAnnotation()]
	if storedListenerPort != "" && storedListenerPort != desiredListenerPort {
		if err := r.setGatewayProgressing(ctx, gateway, gatewayListener, "Listener port changed; replacing the controller-owned OpenStack resource graph"); err != nil {
			return ctrl.Result{}, err
		}
		if scope.statusChanged() {
			return ctrl.Result{Requeue: true}, nil
		}
		return r.cleanupGateway(ctx, scope)
	}
	storedClusterID := gateway.Annotations[r.Config.GatewayClusterIDAnnotation()]
	storedOpenStackProjectID := gateway.Annotations[r.Config.GatewayProjectIDAnnotation()]
	if !controllerutil.ContainsFinalizer(gateway, r.Config.GatewayFinalizer()) || storedListenerPort == "" || storedClusterID == "" || storedOpenStackProjectID == "" {
		if err := r.bindGateway(ctx, gateway, desiredListenerPort); err != nil {
			if errors.Is(err, controller.ErrUnsupportedGatewayAPIVersion) {
				return r.setGatewayUnsupportedVersion(ctx, scope)
			}
			return ctrl.Result{}, err
		}
		logger.V(1).Info("Added finalizer and OpenStack resource binding to Gateway")
		return ctrl.Result{Requeue: true}, nil
	}
	gatewayResult, err := r.ensureGateway(ctx, gateway)
	if err != nil {
		if errors.Is(err, controller.ErrUnsupportedGatewayAPIVersion) {
			return r.setGatewayUnsupportedVersion(ctx, scope)
		}
		if errors.Is(err, errGatewayChanged) {
			return ctrl.Result{}, err
		}
		return r.handleGatewayProviderFailure(ctx, scope, gatewayListener, err)
	}
	if gatewayResult.Outcome.State == cloud.OutcomeProgressing {
		message := controller.ProviderProgressMessage(gatewayResult.Outcome, "Octavia resources for the Gateway are still progressing")
		if err := r.setGatewayProgressing(ctx, gateway, gatewayListener, message); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: controller.ProviderProgressRequeueAfter(gatewayResult.Outcome, gateway.UID)}, nil
	}
	gatewayState := gatewayResult.State
	logger.V(1).Info("Ensured OpenStack resources for Gateway", "loadBalancerID", gatewayState.LoadBalancerID, "listenerID", gatewayState.ListenerID)
	if err := r.setGatewayProgrammed(ctx, gateway, gatewayListener, gatewayState); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: controller.OpenStackResyncAfter(r.Config.OpenStackResyncInterval, gateway.UID)}, nil
}
