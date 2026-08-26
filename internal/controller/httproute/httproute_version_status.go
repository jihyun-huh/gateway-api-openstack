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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
)

func (r *Reconciler) setRouteUnsupportedVersion(
	ctx context.Context,
	scope *httpRouteScope,
) (ctrl.Result, error) {
	route := scope.route
	scope.statusPlan = nil
	if r.APIReader == nil {
		return ctrl.Result{}, controller.ErrAPIReaderRequired
	}
	var current gatewayv1.HTTPRoute
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(route), &current); err != nil {
		if apierrors.IsNotFound(err) {
			scope.skipPatch = true
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !sameHTTPRouteRevision(&current, route.UID, route.Generation, false) {
		scope.skipPatch = true
		return ctrl.Result{}, nil
	}
	parents, err := r.managedParentsWithReader(ctx, r.APIReader, &current)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(parents) == 0 {
		scope.skipPatch = true
		return ctrl.Result{}, nil
	}
	liveClassUnsupported := false
	for _, parent := range parents {
		if !controller.GatewayClassSupportsInstalledVersion(parent.gatewayClass) {
			liveClassUnsupported = true
			break
		}
	}
	recheckVersion := false
	if !liveClassUnsupported {
		versionErr := controller.ValidateInstalledGatewayAPIVersion(ctx, r.APIReader)
		if versionErr == nil {
			return ctrl.Result{}, errHTTPRouteChanged
		}
		if !errors.Is(versionErr, controller.ErrUnsupportedGatewayAPIVersion) {
			return ctrl.Result{}, versionErr
		}
		recheckVersion = true
	}
	updates := make([]parentStatusUpdate, 0, len(parents)+1)
	for _, parent := range parents {
		updates = append(updates, parentStatusUpdate{
			parent: parent.ref,
			status: r.unsupportedGatewayAPIVersionRouteStatus(&current, parent.ref),
		})
	}
	*route = current
	scope.setStatuses(updates)
	result := ctrl.Result{}
	if recheckVersion {
		result.RequeueAfter = controller.GatewayAPIVersionRequeueAfter(route.UID)
	}
	return result, nil
}

func (r *Reconciler) unsupportedGatewayAPIVersionRouteStatus(
	route *gatewayv1.HTTPRoute,
	parentRef gatewayv1.ParentReference,
) routeReconcileStatus {
	status := unsupportedGatewayAPIVersionRouteStatus()
	for _, parent := range route.Status.Parents {
		if parent.ControllerName != r.Config.ControllerName ||
			!controller.ParentRefsEqual(parent.ParentRef, route.Namespace, parentRef, route.Namespace) {
			continue
		}
		programmed := meta.FindStatusCondition(parent.Conditions, r.Config.Domain()+"/Programmed")
		if programmed != nil && programmed.Status == metav1.ConditionTrue &&
			programmed.ObservedGeneration == route.Generation {
			status.programmed = programmed.Status
			status.programmedReason = programmed.Reason
			status.programmedMessage = programmed.Message
		}
		break
	}
	return status
}
