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

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

type gatewayClassScope struct {
	gatewayClass *gatewayv1.GatewayClass
	original     *gatewayv1.GatewayClass
}

func (s *gatewayClassScope) finalizerChanged() bool {
	return !equality.Semantic.DeepEqual(s.original.Finalizers, s.gatewayClass.Finalizers)
}

func (r *GatewayClassReconciler) newGatewayClassScope(
	ctx context.Context,
	req ctrl.Request,
) (*gatewayClassScope, bool, error) {
	gatewayClass := &gatewayv1.GatewayClass{}
	if err := r.Get(ctx, req.NamespacedName, gatewayClass); err != nil {
		return nil, false, client.IgnoreNotFound(err)
	}
	if gatewayClass.Spec.ControllerName != r.Config.ControllerName {
		log.FromContext(ctx).V(4).Info(
			"Ignoring GatewayClass assigned to another controller",
			"controllerName", gatewayClass.Spec.ControllerName,
		)
		return nil, false, nil
	}
	return &gatewayClassScope{
		gatewayClass: gatewayClass,
		original:     gatewayClass.DeepCopy(),
	}, true, nil
}

func (r *GatewayClassReconciler) patchGatewayClass(ctx context.Context, scope *gatewayClassScope) error {
	desired := scope.gatewayClass.DeepCopy()
	metadataChanged := scope.finalizerChanged()
	statusChanged := !equality.Semantic.DeepEqual(scope.original.Status, desired.Status)

	if metadataChanged {
		metadataBase := scope.original.DeepCopy()
		metadataTarget := metadataBase.DeepCopy()
		metadataTarget.Finalizers = append([]string(nil), desired.Finalizers...)
		if err := r.Patch(ctx, metadataTarget, optimisticMergeFrom(metadataBase)); err != nil {
			return fmt.Errorf("patch GatewayClass metadata: %w", err)
		}
		desired.ResourceVersion = metadataTarget.ResourceVersion
		log.FromContext(ctx).V(1).Info(
			"Updated GatewaysExist finalizer on GatewayClass",
			"added", controllerutil.ContainsFinalizer(desired, gatewayv1.GatewayClassFinalizerGatewaysExist),
		)
	}

	if statusChanged {
		statusBase := desired.DeepCopy()
		statusBase.Status = scope.original.Status
		statusTarget := desired.DeepCopy()
		if err := r.Status().Patch(ctx, statusTarget, optimisticMergeFrom(statusBase)); err != nil {
			return fmt.Errorf("patch GatewayClass status: %w", err)
		}
		*scope.gatewayClass = *statusTarget
		accepted := gatewayClassConditionStatus(
			statusTarget.Status.Conditions,
			gatewayv1.GatewayClassConditionStatusAccepted,
		)
		supportedVersion := gatewayClassConditionStatus(
			statusTarget.Status.Conditions,
			gatewayv1.GatewayClassConditionStatusSupportedVersion,
		)
		log.FromContext(ctx).V(1).Info(
			"Updated GatewayClass status",
			"accepted", accepted,
			"supportedVersion", supportedVersion,
		)
	}
	return nil
}

func gatewayClassConditionStatus(
	conditions []metav1.Condition,
	conditionType gatewayv1.GatewayClassConditionType,
) metav1.ConditionStatus {
	for _, condition := range conditions {
		if condition.Type == string(conditionType) {
			return condition.Status
		}
	}
	return metav1.ConditionUnknown
}
