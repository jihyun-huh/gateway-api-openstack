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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

type gatewayProviderWarning struct {
	policy    providerFailurePolicy
	operation string
}

type gatewayScope struct {
	gateway      *gatewayv1.Gateway
	original     *gatewayv1.Gateway
	gatewayClass *gatewayv1.GatewayClass
	managed      bool
	skipPatch    bool
	warnings     []gatewayProviderWarning
	patchCause   error
}

func (r *GatewayReconciler) newGatewayScope(
	ctx context.Context,
	req ctrl.Request,
) (*gatewayScope, bool, error) {
	gateway := &gatewayv1.Gateway{}
	if err := r.Get(ctx, req.NamespacedName, gateway); err != nil {
		return nil, false, client.IgnoreNotFound(err)
	}
	scope := &gatewayScope{gateway: gateway, original: gateway.DeepCopy()}
	if !gateway.DeletionTimestamp.IsZero() {
		return scope, gatewayHasControllerBinding(r.Config, gateway), nil
	}
	gatewayClass, managed, err := r.getGatewayClass(ctx, gateway)
	if err != nil {
		return nil, false, err
	}
	scope.gatewayClass = gatewayClass
	scope.managed = managed
	responsible := managed || gatewayHasControllerBinding(r.Config, gateway)
	return scope, responsible, nil
}

func (s *gatewayScope) refresh(gateway *gatewayv1.Gateway) error {
	desiredStatus := s.gateway.DeepCopy().Status
	statusChanged := !equality.Semantic.DeepEqual(s.original.Status, desiredStatus)
	if statusChanged && !equality.Semantic.DeepEqual(s.original.Status, gateway.Status) {
		return errGatewayChanged
	}
	s.gateway = gateway.DeepCopy()
	s.original = gateway.DeepCopy()
	if statusChanged {
		s.gateway.Status = desiredStatus
	}
	return nil
}

func (s *gatewayScope) queueWarning(policy providerFailurePolicy, operation string) {
	s.warnings = append(s.warnings, gatewayProviderWarning{policy: policy, operation: operation})
}

func (s *gatewayScope) statusChanged() bool {
	return !equality.Semantic.DeepEqual(s.original.Status, s.gateway.Status)
}

func (r *GatewayReconciler) patchGateway(ctx context.Context, scope *gatewayScope) error {
	if scope.skipPatch {
		return nil
	}
	desired := scope.gateway.DeepCopy()
	if !equality.Semantic.DeepEqual(scope.original.Status, desired.Status) {
		statusBase := desired.DeepCopy()
		statusBase.Status = scope.original.Status
		statusTarget := desired.DeepCopy()
		if err := r.Status().Patch(ctx, statusTarget, optimisticMergeFrom(statusBase)); err != nil {
			return fmt.Errorf("patch Gateway status: %w", err)
		}
		desired = statusTarget
		scope.original = statusTarget.DeepCopy()
	}
	*scope.gateway = *desired
	warnings := scope.warnings
	scope.warnings = nil
	for _, warning := range warnings {
		recordProviderWarning(r.Recorder, scope.gateway, warning.policy, warning.operation)
	}
	return nil
}

func (r *GatewayReconciler) patchGatewayMetadata(ctx context.Context, scope *gatewayScope) error {
	if !gatewayOwnedMetadataChanged(r.Config, scope.original, scope.gateway) {
		return nil
	}
	desiredStatus := scope.gateway.DeepCopy().Status
	metadataBase := scope.original.DeepCopy()
	metadataTarget := metadataBase.DeepCopy()
	copyGatewayOwnedMetadata(r.Config, metadataTarget, scope.gateway)
	if err := r.Patch(ctx, metadataTarget, optimisticMergeFrom(metadataBase)); err != nil {
		return fmt.Errorf("patch Gateway metadata: %w", err)
	}
	scope.gateway = metadataTarget.DeepCopy()
	scope.original = metadataTarget.DeepCopy()
	scope.gateway.Status = desiredStatus
	return nil
}

func gatewayOwnedMetadataChanged(config Config, left, right *gatewayv1.Gateway) bool {
	if controllerutil.ContainsFinalizer(left, config.gatewayFinalizer()) !=
		controllerutil.ContainsFinalizer(right, config.gatewayFinalizer()) {
		return true
	}
	for _, key := range gatewayBindingAnnotationKeys(config) {
		if left.Annotations[key] != right.Annotations[key] {
			return true
		}
	}
	return false
}

func copyGatewayOwnedMetadata(config Config, target, desired *gatewayv1.Gateway) {
	if controllerutil.ContainsFinalizer(desired, config.gatewayFinalizer()) {
		controllerutil.AddFinalizer(target, config.gatewayFinalizer())
	} else {
		controllerutil.RemoveFinalizer(target, config.gatewayFinalizer())
	}
	if target.Annotations == nil {
		target.Annotations = map[string]string{}
	}
	for _, key := range gatewayBindingAnnotationKeys(config) {
		if value := desired.Annotations[key]; value != "" {
			target.Annotations[key] = value
		} else {
			delete(target.Annotations, key)
		}
	}
}

func gatewayBindingAnnotationKeys(config Config) []string {
	return []string{
		config.gatewayListenerPortAnnotation(),
		config.gatewayClusterIDAnnotation(),
		config.gatewayProjectIDAnnotation(),
	}
}
