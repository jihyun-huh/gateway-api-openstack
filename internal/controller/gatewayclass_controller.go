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

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// GatewayClassReconciler publishes the capabilities of GatewayClasses assigned
// to this controller and prevents a class from being removed while Gateways
// still reference it.
type GatewayClassReconciler struct {
	client.Client
	Config Config
	logger logr.Logger
}

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch
func (r *GatewayClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.logger = log.FromContext(ctx).WithValues("gatewayClass", req.Name)
	ctx = log.IntoContext(ctx, r.logger)
	r.logger.V(4).Info("Reconciling GatewayClass")

	var gatewayClass gatewayv1.GatewayClass
	if err := r.Get(ctx, req.NamespacedName, &gatewayClass); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if gatewayClass.Spec.ControllerName != r.Config.ControllerName {
		r.logger.V(4).Info("Ignoring GatewayClass assigned to another controller", "controllerName", gatewayClass.Spec.ControllerName)
		return ctrl.Result{}, nil
	}

	hasGateways, err := r.hasReferencingGateways(ctx, gatewayClass.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list Gateways referencing GatewayClass: %w", err)
	}
	metadataBase := gatewayClass.DeepCopy()
	hadFinalizer := controllerutil.ContainsFinalizer(&gatewayClass, gatewayv1.GatewayClassFinalizerGatewaysExist)
	if hasGateways {
		controllerutil.AddFinalizer(&gatewayClass, gatewayv1.GatewayClassFinalizerGatewaysExist)
	} else {
		controllerutil.RemoveFinalizer(&gatewayClass, gatewayv1.GatewayClassFinalizerGatewaysExist)
	}
	if !equality.Semantic.DeepEqual(metadataBase.Finalizers, gatewayClass.Finalizers) {
		if err := r.Patch(ctx, &gatewayClass, optimisticMergeFrom(metadataBase)); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch GatewayClass finalizer: %w", err)
		}
		r.logger.V(1).Info("Updated GatewaysExist finalizer on GatewayClass", "added", !hadFinalizer)
		// Removing the last finalizer from a deleting class can delete it before a
		// status update is possible.
		if !hasGateways && !gatewayClass.DeletionTimestamp.IsZero() {
			return ctrl.Result{}, nil
		}
	}

	statusBase := gatewayClass.DeepCopy()
	if gatewayClass.Spec.ParametersRef != nil {
		gatewayClass.Status.SupportedFeatures = nil
		setCondition(&gatewayClass.Status.Conditions, condition(
			string(gatewayv1.GatewayClassConditionStatusAccepted),
			metav1.ConditionFalse,
			string(gatewayv1.GatewayClassReasonInvalidParameters),
			"GatewayClass parametersRef is not supported in Phase 1",
			gatewayClass.Generation,
		))
	} else {
		gatewayClass.Status.SupportedFeatures = phase1SupportedFeatures()
		setCondition(&gatewayClass.Status.Conditions, condition(
			string(gatewayv1.GatewayClassConditionStatusAccepted),
			metav1.ConditionTrue,
			string(gatewayv1.GatewayClassReasonAccepted),
			"GatewayClass is accepted",
			gatewayClass.Generation,
		))
	}
	if equality.Semantic.DeepEqual(statusBase.Status, gatewayClass.Status) {
		return ctrl.Result{}, nil
	}
	if err := r.Status().Patch(ctx, &gatewayClass, optimisticMergeFrom(statusBase)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch GatewayClass status: %w", err)
	}
	r.logger.V(1).Info("Updated GatewayClass status", "accepted", gatewayClass.Spec.ParametersRef == nil)
	return ctrl.Result{}, nil
}

func (r *GatewayClassReconciler) hasReferencingGateways(ctx context.Context, className string) (bool, error) {
	var gateways gatewayv1.GatewayList
	if err := r.List(ctx, &gateways); err != nil {
		return false, err
	}
	for _, gateway := range gateways.Items {
		if gateway.Spec.GatewayClassName == gatewayv1.ObjectName(className) {
			return true, nil
		}
	}
	return false, nil
}

func phase1SupportedFeatures() []gatewayv1.SupportedFeature {
	// Gateway and HTTPRoute feature names represent their complete core
	// conformance profiles. Phase 1 intentionally implements a smaller subset,
	// so publishing either here would overstate tested support.
	return nil
}

func gatewayClassRequestsForGateway(_ context.Context, object client.Object) []reconcile.Request {
	gateway, ok := object.(*gatewayv1.Gateway)
	if !ok {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)},
	}}
}

// SetupWithManager registers GatewayClass reconciliation and requeues a class
// whenever one of its referencing Gateways changes.
func (r *GatewayClassReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		For(&gatewayv1.GatewayClass{}).
		Watches(
			&gatewayv1.Gateway{},
			handler.EnqueueRequestsFromMapFunc(gatewayClassRequestsForGateway),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Named("gatewayclass").
		Complete(r)
}
