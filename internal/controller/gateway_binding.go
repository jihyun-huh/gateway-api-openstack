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

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

func (r *GatewayReconciler) bindGateway(
	ctx context.Context,
	expected *gatewayv1.Gateway,
	desiredListenerPort string,
) error {
	if r.APIReader == nil {
		return errAPIReaderRequired
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
	if !gatewayClassSupportsInstalledVersion(&gatewayClass) {
		return errUnsupportedGatewayAPIVersion
	}
	if err := validateGatewayBinding(r.Config, &gateway); err != nil {
		return errors.Join(errGatewayChanged, err)
	}
	listener, validationErr := validateGateway(&gateway)
	if validationErr != nil || strconv.Itoa(int(listener.Port)) != desiredListenerPort {
		return errGatewayChanged
	}

	metadataBase := gateway.DeepCopy()
	if gateway.Annotations == nil {
		gateway.Annotations = map[string]string{}
	}
	gateway.Annotations[r.Config.gatewayListenerPortAnnotation()] = desiredListenerPort
	gateway.Annotations[r.Config.gatewayClusterIDAnnotation()] = r.Config.ClusterID
	gateway.Annotations[r.Config.gatewayProjectIDAnnotation()] = r.Config.OpenStackProjectID
	controllerutil.AddFinalizer(&gateway, r.Config.gatewayFinalizer())
	if equality.Semantic.DeepEqual(metadataBase.ObjectMeta, gateway.ObjectMeta) {
		return nil
	}
	if err := r.validateGatewayMutationOwnership(ctx, metadataBase, false); err != nil {
		return err
	}
	if err := r.Patch(ctx, &gateway, optimisticMergeFrom(metadataBase)); err != nil {
		return fmt.Errorf("patch Gateway resource binding: %w", err)
	}
	return nil
}

func (r *GatewayReconciler) validateGatewayMutationOwnership(
	ctx context.Context,
	expected *gatewayv1.Gateway,
	requireBinding bool,
) error {
	if err := validateInstalledGatewayAPIVersion(ctx, r.APIReader); err != nil {
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
	if !gatewayBindingMetadataEqual(r.Config, &gateway, expected) {
		return errGatewayChanged
	}
	if requireBinding {
		if !controllerutil.ContainsFinalizer(&gateway, r.Config.gatewayFinalizer()) ||
			gateway.Annotations[r.Config.gatewayListenerPortAnnotation()] == "" ||
			gateway.Annotations[r.Config.gatewayClusterIDAnnotation()] == "" ||
			gateway.Annotations[r.Config.gatewayProjectIDAnnotation()] == "" ||
			validateGatewayBinding(r.Config, &gateway) != nil {
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
	if !gatewayClassSupportsInstalledVersion(&gatewayClass) {
		return errUnsupportedGatewayAPIVersion
	}
	return nil
}

func validateGatewayBinding(config Config, gateway *gatewayv1.Gateway) error {
	bindingValues := []string{
		gateway.Annotations[config.gatewayListenerPortAnnotation()],
		gateway.Annotations[config.gatewayClusterIDAnnotation()],
		gateway.Annotations[config.gatewayProjectIDAnnotation()],
	}
	populatedBindingCount := 0
	for _, value := range bindingValues {
		if value != "" {
			populatedBindingCount++
		}
	}
	if populatedBindingCount != len(bindingValues) && (populatedBindingCount != 0 || controllerutil.ContainsFinalizer(gateway, config.gatewayFinalizer())) {
		return fmt.Errorf("gateway %s/%s has an incomplete stored OpenStack resource binding; restore the original annotations before reconciling", gateway.Namespace, gateway.Name)
	}
	if stored := gateway.Annotations[config.gatewayClusterIDAnnotation()]; stored != "" && stored != config.ClusterID {
		return fmt.Errorf("controller cluster identity differs from this Gateway's existing resource binding; restore the original cluster ID before reconciling")
	}
	if stored := gateway.Annotations[config.gatewayProjectIDAnnotation()]; stored != "" && stored != config.OpenStackProjectID {
		return fmt.Errorf("authenticated OpenStack project differs from this Gateway's existing resource binding; restore access to the original project before reconciling")
	}
	return nil
}

func gatewayHasControllerBinding(config Config, gateway *gatewayv1.Gateway) bool {
	if controllerutil.ContainsFinalizer(gateway, config.gatewayFinalizer()) {
		return true
	}
	for _, key := range []string{
		config.gatewayListenerPortAnnotation(),
		config.gatewayClusterIDAnnotation(),
		config.gatewayProjectIDAnnotation(),
	} {
		if gateway.Annotations[key] != "" {
			return true
		}
	}
	return false
}

func gatewayBindingMetadataEqual(config Config, left, right *gatewayv1.Gateway) bool {
	if controllerutil.ContainsFinalizer(left, config.gatewayFinalizer()) !=
		controllerutil.ContainsFinalizer(right, config.gatewayFinalizer()) {
		return false
	}
	for _, key := range []string{
		config.gatewayListenerPortAnnotation(),
		config.gatewayClusterIDAnnotation(),
		config.gatewayProjectIDAnnotation(),
	} {
		if left.Annotations[key] != right.Annotations[key] {
			return false
		}
	}
	return true
}

func (r *GatewayReconciler) storedGatewayIdentity(gateway *gatewayv1.Gateway) cloud.Identity {
	return storedGatewayIdentity(r.Config, gateway)
}

func storedGatewayIdentity(config Config, gateway *gatewayv1.Gateway) cloud.Identity {
	identity := gatewayIdentity(config, gateway)
	if value := gateway.Annotations[config.gatewayClusterIDAnnotation()]; value != "" {
		identity.ClusterID = value
	}
	if value := gateway.Annotations[config.gatewayProjectIDAnnotation()]; value != "" {
		identity.OpenStackProjectID = value
	}
	return identity
}
