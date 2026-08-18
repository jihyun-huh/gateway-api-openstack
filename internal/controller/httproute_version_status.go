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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

func (r *HTTPRouteReconciler) setRouteUnsupportedVersion(
	ctx context.Context,
	scope *httpRouteScope,
) (ctrl.Result, error) {
	route := scope.route
	scope.statusPlan = nil
	if r.APIReader == nil {
		return ctrl.Result{}, errAPIReaderRequired
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
		if !gatewayClassSupportsInstalledVersion(parent.gatewayClass) {
			liveClassUnsupported = true
			break
		}
	}
	recheckVersion := false
	if !liveClassUnsupported {
		versionErr := validateInstalledGatewayAPIVersion(ctx, r.APIReader)
		if versionErr == nil {
			return ctrl.Result{}, errHTTPRouteChanged
		}
		if !errors.Is(versionErr, errUnsupportedGatewayAPIVersion) {
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
		result.RequeueAfter = gatewayAPIVersionRequeueAfter(route.UID)
	}
	return result, nil
}

func (r *HTTPRouteReconciler) unsupportedGatewayAPIVersionRouteStatus(
	route *gatewayv1.HTTPRoute,
	parentRef gatewayv1.ParentReference,
) routeReconcileStatus {
	status := unsupportedGatewayAPIVersionRouteStatus()
	for _, parent := range route.Status.Parents {
		if parent.ControllerName != r.Config.ControllerName ||
			!parentRefsEqual(parent.ParentRef, route.Namespace, parentRef, route.Namespace) {
			continue
		}
		programmed := meta.FindStatusCondition(parent.Conditions, r.Config.domain()+"/Programmed")
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

func (r *HTTPRouteReconciler) storedRouteCleanupAllowedDuringVersionMismatch(
	ctx context.Context,
	route *gatewayv1.HTTPRoute,
	stored cloud.Identity,
) (bool, error) {
	if r.APIReader == nil {
		return false, errAPIReaderRequired
	}
	var current gatewayv1.HTTPRoute
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(route), &current); err != nil {
		return false, errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if !sameHTTPRouteRevision(&current, route.UID, route.Generation, !route.DeletionTimestamp.IsZero()) {
		return false, errHTTPRouteChanged
	}
	if route.ResourceVersion != "" && current.ResourceVersion != route.ResourceVersion {
		return false, errHTTPRouteChanged
	}

	for _, parentRef := range current.Spec.ParentRefs {
		if !isGatewayParentRef(parentRef) {
			continue
		}
		namespace := current.Namespace
		if parentRef.Namespace != nil {
			namespace = string(*parentRef.Namespace)
		}
		var gateway gatewayv1.Gateway
		if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: string(parentRef.Name)}, &gateway); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, fmt.Errorf("get parent Gateway during Gateway API version check: %w", err)
		}
		var gatewayClass gatewayv1.GatewayClass
		if err := r.APIReader.Get(ctx, types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)}, &gatewayClass); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("get parent GatewayClass during Gateway API version check: %w", err)
		}
		if gatewayClass.Spec.ControllerName != r.Config.ControllerName {
			continue
		}
		boundGatewayIsDeleting := gateway.Namespace == stored.GatewayNamespace &&
			gateway.Name == stored.GatewayName && string(gateway.UID) == stored.GatewayUID &&
			!gateway.DeletionTimestamp.IsZero()
		if !boundGatewayIsDeleting {
			return false, nil
		}
	}
	return true, nil
}
