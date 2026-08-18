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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

// cleanupGateway converges a previously valid Gateway to no OpenStack
// resources before dropping this controller's finalizer. DeleteGateway is
// identity-safe and idempotent, so it is also safe when creation never began.
func (r *GatewayReconciler) cleanupGateway(ctx context.Context, scope *gatewayScope) (ctrl.Result, error) {
	expectedResourceVersion := scope.gateway.ResourceVersion
	var gateway gatewayv1.Gateway
	if err := r.Get(ctx, client.ObjectKeyFromObject(scope.gateway), &gateway); err != nil {
		if apierrors.IsNotFound(err) {
			scope.skipPatch = true
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if expectedResourceVersion != "" && gateway.ResourceVersion != expectedResourceVersion {
		return ctrl.Result{}, apierrors.NewConflict(
			schema.GroupResource{Group: gatewayv1.GroupName, Resource: "gateways"},
			gateway.Name,
			errors.New("gateway changed before OpenStack resource cleanup"),
		)
	}
	if err := scope.refresh(&gateway); err != nil {
		return ctrl.Result{}, err
	}
	hasFinalizer := controllerutil.ContainsFinalizer(scope.gateway, r.Config.gatewayFinalizer())
	_, hasListenerPortBinding := scope.gateway.Annotations[r.Config.gatewayListenerPortAnnotation()]
	_, hasClusterIDBinding := scope.gateway.Annotations[r.Config.gatewayClusterIDAnnotation()]
	_, hasProjectIDBinding := scope.gateway.Annotations[r.Config.gatewayProjectIDAnnotation()]
	hasBinding := hasListenerPortBinding || hasClusterIDBinding || hasProjectIDBinding
	if !hasFinalizer && !hasBinding {
		return ctrl.Result{}, nil
	}
	if err := validateGatewayBinding(r.Config, scope.gateway); err != nil {
		return ctrl.Result{}, err
	}
	outcome, err := r.deleteGateway(ctx, scope.gateway)
	if err != nil {
		if errors.Is(err, errUnsupportedGatewayAPIVersion) {
			return r.setGatewayUnsupportedVersion(ctx, scope)
		}
		if errors.Is(err, errGatewayChanged) {
			return ctrl.Result{}, err
		}
		providerErr := fmt.Errorf("delete OpenStack resources for Gateway: %w", err)
		return r.handleGatewayDeleteFailure(scope, providerErr)
	}
	if err := outcome.Validate(); err != nil {
		return ctrl.Result{}, fmt.Errorf("validate Gateway deletion outcome: %w", err)
	}
	if outcome.State == cloud.OutcomeProgressing {
		return ctrl.Result{RequeueAfter: providerProgressRequeueAfter(outcome, scope.gateway.UID)}, nil
	}
	controllerutil.RemoveFinalizer(scope.gateway, r.Config.gatewayFinalizer())
	delete(scope.gateway.Annotations, r.Config.gatewayListenerPortAnnotation())
	delete(scope.gateway.Annotations, r.Config.gatewayClusterIDAnnotation())
	delete(scope.gateway.Annotations, r.Config.gatewayProjectIDAnnotation())
	if err := r.patchGatewayMetadata(ctx, scope); err != nil {
		if apierrors.IsNotFound(err) {
			scope.skipPatch = true
		} else {
			return ctrl.Result{}, fmt.Errorf("remove Gateway resource binding: %w", err)
		}
	}
	if !scope.gateway.DeletionTimestamp.IsZero() {
		scope.skipPatch = true
	}
	return ctrl.Result{}, nil
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
	if err := r.validateGatewayCleanupMutation(ctx, &current); err != nil {
		return cloud.Outcome{}, err
	}

	return r.Provider.DeleteGateway(ctx, r.storedGatewayIdentity(&current))
}

func (r *GatewayReconciler) validateGatewayCleanupMutation(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
) error {
	if !gateway.DeletionTimestamp.IsZero() {
		return nil
	}
	var gatewayClass gatewayv1.GatewayClass
	if err := r.APIReader.Get(
		ctx,
		types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)},
		&gatewayClass,
	); err != nil {
		if apierrors.IsNotFound(err) {
			return validateInstalledGatewayAPIVersion(ctx, r.APIReader)
		}
		return fmt.Errorf("get GatewayClass before Gateway cleanup: %w", err)
	}
	if gatewayClass.Spec.ControllerName != r.Config.ControllerName {
		return nil
	}
	if !gatewayClassSupportsInstalledVersion(&gatewayClass) {
		return errUnsupportedGatewayAPIVersion
	}
	if err := validateInstalledGatewayAPIVersion(ctx, r.APIReader); err != nil {
		return err
	}
	if gatewayClass.Spec.ParametersRef != nil {
		return nil
	}
	listener, validationErr := validateGateway(gateway)
	if validationErr != nil {
		return nil
	}
	storedListenerPort := gateway.Annotations[r.Config.gatewayListenerPortAnnotation()]
	if storedListenerPort != "" && storedListenerPort != strconv.Itoa(int(listener.Port)) {
		return nil
	}
	return errGatewayChanged
}
