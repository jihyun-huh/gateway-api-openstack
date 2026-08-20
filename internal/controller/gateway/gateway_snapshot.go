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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
)

type gatewayBindingRequirement int

const (
	gatewayBindingOptional gatewayBindingRequirement = iota
	gatewayBindingRequired
)

// gatewayMutationSnapshot contains the live Kubernetes inputs that authorize
// one Gateway metadata or provider mutation. It is request-local and must not
// be retained across reconciliations.
type gatewayMutationSnapshot struct {
	gateway      *gatewayv1.Gateway
	gatewayClass *gatewayv1.GatewayClass
	listener     *gatewayv1.Listener
	binding      gatewayBindingRequirement
}

// observeGatewayMutationSnapshot builds the model used by one mutation
// attempt. sealGatewayMutationSnapshot must still run after model construction
// so a controller handoff or binding change cannot cross the mutation fence.
func (r *Reconciler) observeGatewayMutationSnapshot(
	ctx context.Context,
	expected *gatewayv1.Gateway,
	binding gatewayBindingRequirement,
	expectedListenerPort string,
) (*gatewayMutationSnapshot, error) {
	if r.APIReader == nil {
		return nil, controller.ErrAPIReaderRequired
	}

	var gateway gatewayv1.Gateway
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(expected), &gateway); err != nil {
		return nil, errors.Join(errGatewayChanged, client.IgnoreNotFound(err))
	}
	if gateway.UID != expected.UID || gateway.Generation != expected.Generation ||
		!gateway.DeletionTimestamp.IsZero() {
		return nil, errGatewayChanged
	}

	var gatewayClass gatewayv1.GatewayClass
	if err := r.APIReader.Get(
		ctx,
		types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)},
		&gatewayClass,
	); err != nil {
		return nil, errors.Join(errGatewayChanged, client.IgnoreNotFound(err))
	}
	if gatewayClass.Spec.ControllerName != r.Config.ControllerName || gatewayClass.Spec.ParametersRef != nil {
		return nil, errGatewayChanged
	}
	if !controller.GatewayClassSupportsInstalledVersion(&gatewayClass) {
		return nil, controller.ErrUnsupportedGatewayAPIVersion
	}
	if binding == gatewayBindingRequired && !controllerutil.ContainsFinalizer(&gateway, r.Config.GatewayFinalizer()) {
		return nil, errGatewayChanged
	}
	if err := controller.ValidateGatewayBinding(r.Config, &gateway); err != nil {
		return nil, errors.Join(errGatewayChanged, err)
	}

	listener, validationErr := Validate(&gateway)
	if validationErr != nil {
		return nil, errGatewayChanged
	}
	listenerPort := strconv.Itoa(int(listener.Port))
	if expectedListenerPort != "" && listenerPort != expectedListenerPort {
		return nil, errGatewayChanged
	}
	if binding == gatewayBindingRequired &&
		gateway.Annotations[r.Config.GatewayListenerPortAnnotation()] != listenerPort {
		return nil, errGatewayChanged
	}

	return &gatewayMutationSnapshot{
		gateway:      &gateway,
		gatewayClass: &gatewayClass,
		listener:     listener,
		binding:      binding,
	}, nil
}

// sealGatewayMutationSnapshot rechecks the small ownership fingerprint used by
// the observed model immediately before mutation. The second read is a safety
// fence, not a cache refresh that callers may omit.
func (r *Reconciler) sealGatewayMutationSnapshot(
	ctx context.Context,
	snapshot *gatewayMutationSnapshot,
) error {
	if err := controller.ValidateInstalledGatewayAPIVersion(ctx, r.APIReader); err != nil {
		return err
	}

	var gateway gatewayv1.Gateway
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(snapshot.gateway), &gateway); err != nil {
		return errors.Join(errGatewayChanged, client.IgnoreNotFound(err))
	}
	if gateway.UID != snapshot.gateway.UID || gateway.Generation != snapshot.gateway.Generation ||
		!gateway.DeletionTimestamp.IsZero() ||
		!controller.GatewayBindingMetadataEqual(r.Config, &gateway, snapshot.gateway) {
		return errGatewayChanged
	}
	if snapshot.binding == gatewayBindingRequired {
		if !controllerutil.ContainsFinalizer(&gateway, r.Config.GatewayFinalizer()) ||
			gateway.Annotations[r.Config.GatewayListenerPortAnnotation()] == "" ||
			gateway.Annotations[r.Config.GatewayClusterIDAnnotation()] == "" ||
			gateway.Annotations[r.Config.GatewayProjectIDAnnotation()] == "" ||
			controller.ValidateGatewayBinding(r.Config, &gateway) != nil {
			return errGatewayChanged
		}
	}

	if gateway.Spec.GatewayClassName != gatewayv1.ObjectName(snapshot.gatewayClass.Name) {
		return errGatewayChanged
	}
	var gatewayClass gatewayv1.GatewayClass
	if err := r.APIReader.Get(
		ctx,
		types.NamespacedName{Name: snapshot.gatewayClass.Name},
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

type gatewayReconcilePhase int

const (
	gatewayPhaseIgnore gatewayReconcilePhase = iota
	gatewayPhaseCleanupUnmanaged
	gatewayPhaseUnsupportedVersion
	gatewayPhaseInvalidBinding
	gatewayPhaseUnsupportedParameters
	gatewayPhaseInvalidSpec
	gatewayPhaseReplaceGraph
	gatewayPhaseBind
	gatewayPhaseEnsure
)

// gatewayPhaseDecision is a pure classification of the cached inputs for one
// normal reconciliation. Mutation helpers build their own live snapshots and
// do not treat this decision as mutation authority.
type gatewayPhaseDecision struct {
	phase        gatewayReconcilePhase
	listener     *gatewayv1.Listener
	listenerPort string
	validation   error
}

func (r *Reconciler) decideGatewayPhase(scope *gatewayScope) gatewayPhaseDecision {
	gateway := scope.gateway
	if !scope.managed {
		if controller.GatewayHasControllerBinding(r.Config, gateway) {
			return gatewayPhaseDecision{phase: gatewayPhaseCleanupUnmanaged}
		}
		return gatewayPhaseDecision{phase: gatewayPhaseIgnore}
	}
	if scope.gatewayClass != nil && !controller.GatewayClassSupportsInstalledVersion(scope.gatewayClass) {
		return gatewayPhaseDecision{phase: gatewayPhaseUnsupportedVersion}
	}
	if err := controller.ValidateGatewayBinding(r.Config, gateway); err != nil {
		return gatewayPhaseDecision{phase: gatewayPhaseInvalidBinding, validation: err}
	}
	if scope.gatewayClass != nil && scope.gatewayClass.Spec.ParametersRef != nil {
		return gatewayPhaseDecision{phase: gatewayPhaseUnsupportedParameters}
	}

	listener, err := Validate(gateway)
	if err != nil {
		return gatewayPhaseDecision{phase: gatewayPhaseInvalidSpec, validation: err}
	}
	listenerPort := strconv.Itoa(int(listener.Port))
	storedListenerPort := gateway.Annotations[r.Config.GatewayListenerPortAnnotation()]
	if storedListenerPort != "" && storedListenerPort != listenerPort {
		return gatewayPhaseDecision{
			phase: gatewayPhaseReplaceGraph, listener: listener, listenerPort: listenerPort,
		}
	}
	if !r.gatewayBindingComplete(gateway) {
		return gatewayPhaseDecision{phase: gatewayPhaseBind, listener: listener, listenerPort: listenerPort}
	}
	return gatewayPhaseDecision{phase: gatewayPhaseEnsure, listener: listener, listenerPort: listenerPort}
}

func (r *Reconciler) gatewayBindingComplete(gateway *gatewayv1.Gateway) bool {
	return controllerutil.ContainsFinalizer(gateway, r.Config.GatewayFinalizer()) &&
		gateway.Annotations[r.Config.GatewayListenerPortAnnotation()] != "" &&
		gateway.Annotations[r.Config.GatewayClusterIDAnnotation()] != "" &&
		gateway.Annotations[r.Config.GatewayProjectIDAnnotation()] != ""
}

type gatewayCleanupDisposition string

const (
	gatewayCleanupDelete gatewayCleanupDisposition = "Delete"
	gatewayCleanupHold   gatewayCleanupDisposition = "Hold"
)

// gatewayCleanupSnapshot records the live binding and the cleanup authority
// derived from the current Gateway and GatewayClass lifecycle.
type gatewayCleanupSnapshot struct {
	identity    cloud.Identity
	disposition gatewayCleanupDisposition
}

func (r *Reconciler) observeGatewayCleanupSnapshot(
	ctx context.Context,
	expected *gatewayv1.Gateway,
) (*gatewayCleanupSnapshot, error) {
	if r.APIReader == nil {
		return nil, controller.ErrAPIReaderRequired
	}

	var gateway gatewayv1.Gateway
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(expected), &gateway); err != nil {
		return nil, errors.Join(errGatewayChanged, client.IgnoreNotFound(err))
	}
	if gateway.UID != expected.UID || gateway.Generation != expected.Generation ||
		gateway.ResourceVersion != expected.ResourceVersion ||
		gateway.DeletionTimestamp.IsZero() != expected.DeletionTimestamp.IsZero() {
		return nil, errGatewayChanged
	}
	if err := controller.ValidateGatewayBinding(r.Config, &gateway); err != nil {
		return nil, errors.Join(errGatewayChanged, err)
	}

	snapshot := &gatewayCleanupSnapshot{
		identity:    controller.StoredGatewayIdentity(r.Config, &gateway),
		disposition: gatewayCleanupDelete,
	}
	if !gateway.DeletionTimestamp.IsZero() {
		return snapshot, nil
	}

	var gatewayClass gatewayv1.GatewayClass
	if err := r.APIReader.Get(
		ctx,
		types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)},
		&gatewayClass,
	); err != nil {
		if apierrors.IsNotFound(err) {
			if versionErr := controller.ValidateInstalledGatewayAPIVersion(ctx, r.APIReader); versionErr != nil {
				return nil, versionErr
			}
			return snapshot, nil
		}
		return nil, fmt.Errorf("get GatewayClass before Gateway cleanup: %w", err)
	}
	if gatewayClass.Spec.ControllerName != r.Config.ControllerName {
		return snapshot, nil
	}
	if !controller.GatewayClassSupportsInstalledVersion(&gatewayClass) {
		return nil, controller.ErrUnsupportedGatewayAPIVersion
	}
	if err := controller.ValidateInstalledGatewayAPIVersion(ctx, r.APIReader); err != nil {
		return nil, err
	}
	if gatewayClass.Spec.ParametersRef != nil {
		return snapshot, nil
	}
	listener, validationErr := Validate(&gateway)
	if validationErr != nil {
		return snapshot, nil
	}
	storedListenerPort := gateway.Annotations[r.Config.GatewayListenerPortAnnotation()]
	if storedListenerPort != "" && storedListenerPort != strconv.Itoa(int(listener.Port)) {
		return snapshot, nil
	}

	snapshot.disposition = gatewayCleanupHold
	return snapshot, nil
}
