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
	"strconv"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
)

func (r *Reconciler) bindGateway(
	ctx context.Context,
	expected *gatewayv1.Gateway,
	desiredListenerPort string,
) error {
	if r.APIReader == nil {
		return controller.ErrAPIReaderRequired
	}
	var gateway gatewayv1.Gateway
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(expected), &gateway); err != nil {
		return errors.Join(errGatewayChanged, client.IgnoreNotFound(err))
	}
	if gateway.UID != expected.UID || gateway.Generation != expected.Generation || !gateway.DeletionTimestamp.IsZero() {
		return errGatewayChanged
	}
	var gatewayClass gatewayv1.GatewayClass
	if err := r.APIReader.Get(ctx, types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)}, &gatewayClass); err != nil {
		return errors.Join(errGatewayChanged, client.IgnoreNotFound(err))
	}
	if gatewayClass.Spec.ControllerName != r.Config.ControllerName || gatewayClass.Spec.ParametersRef != nil {
		return errGatewayChanged
	}
	if !controller.GatewayClassSupportsInstalledVersion(&gatewayClass) {
		return controller.ErrUnsupportedGatewayAPIVersion
	}
	if err := controller.ValidateGatewayBinding(r.Config, &gateway); err != nil {
		return errors.Join(errGatewayChanged, err)
	}
	listener, validationErr := Validate(&gateway)
	if validationErr != nil || strconv.Itoa(int(listener.Port)) != desiredListenerPort {
		return errGatewayChanged
	}

	metadataBase := gateway.DeepCopy()
	if gateway.Annotations == nil {
		gateway.Annotations = map[string]string{}
	}
	gateway.Annotations[r.Config.GatewayListenerPortAnnotation()] = desiredListenerPort
	gateway.Annotations[r.Config.GatewayClusterIDAnnotation()] = r.Config.ClusterID
	gateway.Annotations[r.Config.GatewayProjectIDAnnotation()] = r.Config.OpenStackProjectID
	controllerutil.AddFinalizer(&gateway, r.Config.GatewayFinalizer())
	if equality.Semantic.DeepEqual(metadataBase.ObjectMeta, gateway.ObjectMeta) {
		return nil
	}
	if err := r.validateGatewayMutationOwnership(ctx, metadataBase, false); err != nil {
		return err
	}
	if err := r.Patch(ctx, &gateway, controller.OptimisticMergeFrom(metadataBase)); err != nil {
		return fmt.Errorf("patch Gateway resource binding: %w", err)
	}
	return nil
}

func (r *Reconciler) validateGatewayMutationOwnership(
	ctx context.Context,
	expected *gatewayv1.Gateway,
	requireBinding bool,
) error {
	if err := controller.ValidateInstalledGatewayAPIVersion(ctx, r.APIReader); err != nil {
		return err
	}
	var gateway gatewayv1.Gateway
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(expected), &gateway); err != nil {
		return errors.Join(errGatewayChanged, client.IgnoreNotFound(err))
	}
	if gateway.UID != expected.UID || gateway.Generation != expected.Generation ||
		!gateway.DeletionTimestamp.IsZero() {
		return errGatewayChanged
	}
	if !controller.GatewayBindingMetadataEqual(r.Config, &gateway, expected) {
		return errGatewayChanged
	}
	if requireBinding {
		if !controllerutil.ContainsFinalizer(&gateway, r.Config.GatewayFinalizer()) ||
			gateway.Annotations[r.Config.GatewayListenerPortAnnotation()] == "" ||
			gateway.Annotations[r.Config.GatewayClusterIDAnnotation()] == "" ||
			gateway.Annotations[r.Config.GatewayProjectIDAnnotation()] == "" ||
			controller.ValidateGatewayBinding(r.Config, &gateway) != nil {
			return errGatewayChanged
		}
	}
	var gatewayClass gatewayv1.GatewayClass
	if err := r.APIReader.Get(
		ctx,
		types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)},
		&gatewayClass,
	); err != nil {
		return errors.Join(errGatewayChanged, client.IgnoreNotFound(err))
	}
	if gatewayClass.Spec.ControllerName != r.Config.ControllerName || gatewayClass.Spec.ParametersRef != nil {
		return errGatewayChanged
	}
	if !controller.GatewayClassSupportsInstalledVersion(&gatewayClass) {
		return controller.ErrUnsupportedGatewayAPIVersion
	}
	return nil
}
