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
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller/graph"
)

// cleanupGateway converges a previously valid Gateway to no OpenStack
// resources before dropping this controller's finalizer. DeleteGateway is
// identity-safe and idempotent, so it is also safe when creation never began.
func (r *Reconciler) cleanupGateway(ctx context.Context, scope *gatewayScope) (ctrl.Result, error) {
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
	hasFinalizer := controllerutil.ContainsFinalizer(scope.gateway, r.Config.GatewayFinalizer())
	_, hasListenerPortBinding := scope.gateway.Annotations[r.Config.GatewayListenerPortAnnotation()]
	_, hasClusterIDBinding := scope.gateway.Annotations[r.Config.GatewayClusterIDAnnotation()]
	_, hasProjectIDBinding := scope.gateway.Annotations[r.Config.GatewayProjectIDAnnotation()]
	hasBinding := hasListenerPortBinding || hasClusterIDBinding || hasProjectIDBinding
	if !hasFinalizer && !hasBinding {
		return ctrl.Result{}, nil
	}
	if err := controller.ValidateGatewayBinding(r.Config, scope.gateway); err != nil {
		return ctrl.Result{}, err
	}
	outcome, err := r.deleteGateway(ctx, scope.gateway)
	if err != nil {
		if errors.Is(err, controller.ErrUnsupportedGatewayAPIVersion) {
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
		return ctrl.Result{RequeueAfter: controller.ProviderProgressRequeueAfter(outcome, scope.gateway.UID)}, nil
	}
	controllerutil.RemoveFinalizer(scope.gateway, r.Config.GatewayFinalizer())
	delete(scope.gateway.Annotations, r.Config.GatewayListenerPortAnnotation())
	delete(scope.gateway.Annotations, r.Config.GatewayClusterIDAnnotation())
	delete(scope.gateway.Annotations, r.Config.GatewayProjectIDAnnotation())
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

func (r *Reconciler) deleteGateway(ctx context.Context, expected *gatewayv1.Gateway) (cloud.Outcome, error) {
	if r.APIReader == nil {
		return cloud.Outcome{}, controller.ErrAPIReaderRequired
	}
	release, err := graph.Acquire(ctx, r.Coordinator, string(expected.UID))
	if err != nil {
		return cloud.Outcome{}, fmt.Errorf("acquire Gateway graph: %w", err)
	}
	defer release()

	snapshot, err := r.observeGatewayCleanupSnapshot(ctx, expected)
	if err != nil {
		return cloud.Outcome{}, err
	}
	if snapshot.disposition != gatewayCleanupDelete {
		return cloud.Outcome{}, errGatewayChanged
	}
	return r.Provider.DeleteGateway(ctx, snapshot.identity)
}
