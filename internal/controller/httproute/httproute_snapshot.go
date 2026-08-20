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
	"fmt"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
	gatewaycontroller "github.com/jihyun-huh/gateway-api-openstack/internal/controller/gateway"
)

type routeSnapshotPurpose int

const (
	routeSnapshotForEnsure routeSnapshotPurpose = iota
	routeSnapshotForBinding
)

type routeValidationDisposition int

const (
	routeValidationUnknown routeValidationDisposition = iota
	routeValidationGatewayPending
	routeValidationBindingRequired
	routeValidationReady
)

type routeBindingRequirement int

const (
	routeBindingOptional routeBindingRequirement = iota
	routeBindingRequired
)

type routeBindingSnapshot struct {
	identity       cloud.Identity
	present        bool
	hasFinalizer   bool
	matchesDesired bool
}

type routeSelectionSnapshot struct {
	slot     routeSlotDecision
	selected bool
}

// routeValidationSnapshot contains one live view of every Kubernetes input
// that authorizes a route graph mutation. It is local to one mutation attempt
// and must not be retained after releasing the Gateway graph lock.
type routeValidationSnapshot struct {
	route             *gatewayv1.HTTPRoute
	gateway           *gatewayv1.Gateway
	gatewayClass      *gatewayv1.GatewayClass
	listener          gatewayv1.Listener
	binding           routeBindingSnapshot
	selection         routeSelectionSnapshot
	desired           cloud.RouteSpec
	gatewayProgrammed bool
	disposition       routeValidationDisposition
}

type routeCleanupDisposition int

const (
	routeCleanupUnknown routeCleanupDisposition = iota
	routeCleanupRequired
	routeCleanupStillDesired
)

type routeCleanupSnapshot struct {
	route       *gatewayv1.HTTPRoute
	identity    cloud.Identity
	disposition routeCleanupDisposition
}

type routeParentObservation struct {
	ref          gatewayv1.ParentReference
	gateway      *gatewayv1.Gateway
	gatewayClass *gatewayv1.GatewayClass
	listener     *gatewayv1.Listener
	managed      bool
}

func (r *Reconciler) buildRouteValidationSnapshot(
	ctx context.Context,
	expectedRoute *gatewayv1.HTTPRoute,
	expectedGateway *gatewayv1.Gateway,
	purpose routeSnapshotPurpose,
) (routeValidationSnapshot, error) {
	snapshot, err := r.observeRouteValidationInputs(ctx, expectedRoute, expectedGateway, purpose)
	if err != nil {
		return routeValidationSnapshot{}, err
	}

	parent, err := r.validateRouteSnapshotParent(&snapshot, purpose)
	if err != nil || snapshot.disposition == routeValidationGatewayPending {
		return snapshot, err
	}
	if purpose == routeSnapshotForBinding && !snapshot.gatewayProgrammed {
		return routeValidationSnapshot{}, errHTTPRouteChanged
	}

	selection, desired, err := r.selectAndBuildRouteModel(ctx, snapshot, parent, purpose)
	if err != nil {
		return routeValidationSnapshot{}, err
	}
	snapshot.selection = selection
	snapshot.desired = desired

	if err := r.finishRouteBindingSnapshot(&snapshot, purpose); err != nil {
		return routeValidationSnapshot{}, err
	}
	return snapshot, nil
}

func (r *Reconciler) observeRouteValidationInputs(
	ctx context.Context,
	expectedRoute *gatewayv1.HTTPRoute,
	expectedGateway *gatewayv1.Gateway,
	purpose routeSnapshotPurpose,
) (routeValidationSnapshot, error) {
	if r.APIReader == nil {
		return routeValidationSnapshot{}, controller.ErrAPIReaderRequired
	}

	var route gatewayv1.HTTPRoute
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(expectedRoute), &route); err != nil {
		return routeValidationSnapshot{}, errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if !sameHTTPRouteRevision(
		&route,
		expectedRoute.UID,
		expectedRoute.Generation,
		!expectedRoute.DeletionTimestamp.IsZero(),
	) {
		return routeValidationSnapshot{}, errHTTPRouteChanged
	}

	var gateway gatewayv1.Gateway
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(expectedGateway), &gateway); err != nil {
		return routeValidationSnapshot{}, errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if gateway.UID != expectedGateway.UID || gateway.Generation != expectedGateway.Generation ||
		!gateway.DeletionTimestamp.IsZero() {
		return routeValidationSnapshot{}, errHTTPRouteChanged
	}

	var gatewayClass gatewayv1.GatewayClass
	if err := r.APIReader.Get(
		ctx,
		types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)},
		&gatewayClass,
	); err != nil {
		return routeValidationSnapshot{}, errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if gatewayClass.Spec.ControllerName != r.Config.ControllerName ||
		gatewayClass.Spec.ParametersRef != nil ||
		(purpose == routeSnapshotForEnsure && len(route.Spec.ParentRefs) != 1) {
		return routeValidationSnapshot{}, errHTTPRouteChanged
	}
	if !controller.GatewayClassSupportsInstalledVersion(&gatewayClass) {
		return routeValidationSnapshot{}, controller.ErrUnsupportedGatewayAPIVersion
	}
	if !controllerutil.ContainsFinalizer(&gateway, r.Config.GatewayFinalizer()) ||
		controller.ValidateGatewayBinding(r.Config, &gateway) != nil {
		return routeValidationSnapshot{}, errHTTPRouteChanged
	}
	if len(route.Spec.ParentRefs) != 1 {
		return routeValidationSnapshot{}, errHTTPRouteChanged
	}
	return routeValidationSnapshot{
		route:        &route,
		gateway:      &gateway,
		gatewayClass: &gatewayClass,
		disposition:  routeValidationReady,
	}, nil
}

func (r *Reconciler) validateRouteSnapshotParent(
	snapshot *routeValidationSnapshot,
	purpose routeSnapshotPurpose,
) (managedRouteParent, error) {
	listener, validationErr := gatewaycontroller.Validate(snapshot.gateway)
	if validationErr != nil {
		if purpose == routeSnapshotForBinding {
			return managedRouteParent{}, errHTTPRouteChanged
		}
		snapshot.disposition = routeValidationGatewayPending
		return managedRouteParent{}, nil
	}
	snapshot.listener = *listener
	parent := managedRouteParent{
		ref:          snapshot.route.Spec.ParentRefs[0],
		gateway:      snapshot.gateway,
		gatewayClass: snapshot.gatewayClass,
		listener:     &snapshot.listener,
	}
	if err := validateRouteParent(snapshot.route, parent); err != nil {
		if purpose == routeSnapshotForBinding {
			return managedRouteParent{}, errHTTPRouteChanged
		}
		return managedRouteParent{}, err
	}
	if snapshot.gateway.Annotations[r.Config.GatewayListenerPortAnnotation()] != strconv.Itoa(int(snapshot.listener.Port)) {
		return managedRouteParent{}, errHTTPRouteChanged
	}
	snapshot.gatewayProgrammed = gatewayProgrammedForGeneration(snapshot.gateway)
	return parent, nil
}

func (r *Reconciler) selectAndBuildRouteModel(
	ctx context.Context,
	snapshot routeValidationSnapshot,
	parent managedRouteParent,
	purpose routeSnapshotPurpose,
) (routeSelectionSnapshot, cloud.RouteSpec, error) {
	structurallySupported := structurallySupportedRoute(snapshot.route)
	if !structurallySupported && purpose == routeSnapshotForBinding {
		return routeSelectionSnapshot{}, cloud.RouteSpec{}, errHTTPRouteChanged
	}
	selection := routeSelectionSnapshot{}
	if structurallySupported {
		decision, err := r.evaluateRouteSlotWithReader(ctx, r.APIReader, snapshot.route)
		if err != nil {
			return routeSelectionSnapshot{}, cloud.RouteSpec{}, routeSnapshotReadError(purpose, err)
		}
		selection.slot = decision
		if !decision.canReserve {
			return routeSelectionSnapshot{}, cloud.RouteSpec{}, routeSnapshotRejection(purpose, decision.rejection)
		}
	}
	selected, err := r.isSelectedRouteWithReader(ctx, r.APIReader, snapshot.route, parent, false)
	if err != nil {
		return routeSelectionSnapshot{}, cloud.RouteSpec{}, routeSnapshotReadError(purpose, err)
	}
	selection.selected = selected
	if !selected {
		return routeSelectionSnapshot{}, cloud.RouteSpec{}, routeSnapshotRejection(purpose, newRouteBuildError(
			routeErrorUnsupported,
			"Phase 1 supports one HTTPRoute per Gateway; an older route is already selected",
		))
	}

	desired, err := r.buildRouteSpecWithReader(ctx, r.APIReader, snapshot.route, snapshot.gateway)
	if err != nil {
		return routeSelectionSnapshot{}, cloud.RouteSpec{}, routeSnapshotReadError(purpose, err)
	}
	return selection, desired, nil
}

func (r *Reconciler) finishRouteBindingSnapshot(
	snapshot *routeValidationSnapshot,
	purpose routeSnapshotPurpose,
) error {
	storedIdentity, bindingPresent, err := r.storedRouteIdentity(snapshot.route)
	if err != nil {
		return err
	}
	desiredIdentity := controller.RouteIdentity(r.Config, snapshot.gateway, snapshot.route)
	binding := routeBindingSnapshot{
		identity:       storedIdentity,
		present:        bindingPresent,
		hasFinalizer:   controllerutil.ContainsFinalizer(snapshot.route, r.Config.RouteFinalizer()),
		matchesDesired: bindingPresent && storedIdentity == desiredIdentity,
	}
	snapshot.binding = binding

	if purpose == routeSnapshotForBinding {
		return r.validateRouteBindingTransition(*snapshot)
	}

	boundToCurrentGateway := binding.matchesDesired && binding.hasFinalizer
	if !snapshot.gatewayProgrammed && !boundToCurrentGateway {
		snapshot.disposition = routeValidationGatewayPending
		return nil
	}
	desiredBinding := snapshot.route.DeepCopy()
	r.applyRouteBinding(desiredBinding, snapshot.gateway)
	if !routeBindingMetadataEqual(snapshot.route, desiredBinding) {
		snapshot.disposition = routeValidationBindingRequired
	}
	return nil
}

// validateRouteBindingTransition prevents a binding checkpoint from adopting
// incomplete metadata or overwriting cleanup authority for another Gateway.
// It is pure so an ensure snapshot can be reused without rebuilding the
// backend model.
func (r *Reconciler) validateRouteBindingTransition(snapshot routeValidationSnapshot) error {
	if !snapshot.gatewayProgrammed {
		return errHTTPRouteChanged
	}
	if !snapshot.binding.present && snapshot.binding.hasFinalizer {
		return fmt.Errorf(
			"HTTPRoute %s has the controller finalizer but no complete stored Gateway identity",
			client.ObjectKeyFromObject(snapshot.route),
		)
	}
	desiredIdentity := controller.RouteIdentity(r.Config, snapshot.gateway, snapshot.route)
	if snapshot.binding.present && !controller.SameGatewayIdentity(snapshot.binding.identity, desiredIdentity) {
		return errHTTPRouteChanged
	}
	return nil
}

func gatewayProgrammedForGeneration(gateway *gatewayv1.Gateway) bool {
	programmed := meta.FindStatusCondition(
		gateway.Status.Conditions,
		string(gatewayv1.GatewayConditionProgrammed),
	)
	return programmed != nil && programmed.Status == metav1.ConditionTrue &&
		programmed.ObservedGeneration == gateway.Generation
}

func routeSnapshotReadError(purpose routeSnapshotPurpose, err error) error {
	if purpose == routeSnapshotForBinding {
		return errors.Join(errHTTPRouteChanged, err)
	}
	return err
}

func routeSnapshotRejection(purpose routeSnapshotPurpose, rejection error) error {
	if purpose == routeSnapshotForBinding {
		return errHTTPRouteChanged
	}
	return rejection
}

// sealRouteMutationSnapshot verifies that the ownership inputs used to build
// a desired graph have not changed before the first Kubernetes or OpenStack
// mutation. The repeated live reads are a mutation fence, not a second model
// build.
func (r *Reconciler) sealRouteMutationSnapshot(
	ctx context.Context,
	snapshot routeValidationSnapshot,
	bindingRequirement routeBindingRequirement,
) error {
	if err := controller.ValidateInstalledGatewayAPIVersion(ctx, r.APIReader); err != nil {
		return err
	}
	var route gatewayv1.HTTPRoute
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(snapshot.route), &route); err != nil {
		return errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if !sameHTTPRouteRevision(
		&route,
		snapshot.route.UID,
		snapshot.route.Generation,
		!snapshot.route.DeletionTimestamp.IsZero(),
	) {
		return errHTTPRouteChanged
	}
	var gateway gatewayv1.Gateway
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(snapshot.gateway), &gateway); err != nil {
		return errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if gateway.UID != snapshot.gateway.UID || gateway.Generation != snapshot.gateway.Generation ||
		!gateway.DeletionTimestamp.IsZero() {
		return errHTTPRouteChanged
	}
	if !controllerutil.ContainsFinalizer(&gateway, r.Config.GatewayFinalizer()) ||
		!controller.GatewayBindingMetadataEqual(r.Config, &gateway, snapshot.gateway) ||
		controller.ValidateGatewayBinding(r.Config, &gateway) != nil {
		return errHTTPRouteChanged
	}
	if bindingRequirement == routeBindingRequired {
		stored, present, err := r.storedRouteIdentity(&route)
		if err != nil {
			return err
		}
		if !present || stored != controller.RouteIdentity(r.Config, &gateway, &route) ||
			!controllerutil.ContainsFinalizer(&route, r.Config.RouteFinalizer()) {
			return errHTTPRouteChanged
		}
	}
	var gatewayClass gatewayv1.GatewayClass
	if err := r.APIReader.Get(
		ctx,
		types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)},
		&gatewayClass,
	); err != nil {
		return errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if gatewayClass.Spec.ControllerName != r.Config.ControllerName ||
		gatewayClass.Spec.ParametersRef != nil {
		return errHTTPRouteChanged
	}
	if !controller.GatewayClassSupportsInstalledVersion(&gatewayClass) {
		return controller.ErrUnsupportedGatewayAPIVersion
	}
	return nil
}

func (r *Reconciler) buildRouteCleanupSnapshot(
	ctx context.Context,
	expected *gatewayv1.HTTPRoute,
	expectedIdentity cloud.Identity,
) (routeCleanupSnapshot, error) {
	if r.APIReader == nil {
		return routeCleanupSnapshot{}, controller.ErrAPIReaderRequired
	}
	var route gatewayv1.HTTPRoute
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(expected), &route); err != nil {
		return routeCleanupSnapshot{}, errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if !sameHTTPRouteRevision(
		&route,
		expected.UID,
		expected.Generation,
		!expected.DeletionTimestamp.IsZero(),
	) || route.ResourceVersion != expected.ResourceVersion {
		return routeCleanupSnapshot{}, errHTTPRouteChanged
	}
	stored, present, err := r.storedRouteIdentity(&route)
	if err != nil {
		return routeCleanupSnapshot{}, err
	}
	if !present || stored != expectedIdentity {
		return routeCleanupSnapshot{}, errHTTPRouteChanged
	}
	snapshot := routeCleanupSnapshot{
		route:       &route,
		identity:    stored,
		disposition: routeCleanupRequired,
	}
	if !route.DeletionTimestamp.IsZero() {
		return snapshot, nil
	}

	parentObservations, err := r.observeRouteParentsForCleanup(ctx, &route)
	if err != nil {
		return routeCleanupSnapshot{}, err
	}
	if routeCleanupAllowedDuringVersionMismatch(parentObservations, stored) {
		return snapshot, nil
	}
	if err := controller.ValidateInstalledGatewayAPIVersion(ctx, r.APIReader); err != nil {
		return routeCleanupSnapshot{}, err
	}
	parentObservations, err = r.observeRouteParentsForCleanup(ctx, &route)
	if err != nil {
		return routeCleanupSnapshot{}, err
	}
	managedParents := managedParentsFromObservations(parentObservations)
	if len(route.Spec.ParentRefs) != 1 || len(managedParents) != 1 {
		return snapshot, nil
	}
	parent := managedParents[0]
	if !controller.GatewayClassSupportsInstalledVersion(parent.gatewayClass) {
		return routeCleanupSnapshot{}, controller.ErrUnsupportedGatewayAPIVersion
	}
	if parent.gatewayClass.Spec.ParametersRef != nil {
		return routeCleanupSnapshot{}, errHTTPRouteChanged
	}
	if validateRouteParent(&route, parent) != nil || !structurallySupportedRoute(&route) {
		return snapshot, nil
	}
	decision, err := r.evaluateRouteSlotWithReader(ctx, r.APIReader, &route)
	if err != nil {
		return routeCleanupSnapshot{}, err
	}
	if !decision.canReserve {
		return snapshot, nil
	}
	selected, err := r.isSelectedRouteWithReader(ctx, r.APIReader, &route, parent, false)
	if err != nil {
		return routeCleanupSnapshot{}, err
	}
	if !selected {
		return snapshot, nil
	}
	desired, err := r.buildRouteSpecWithReader(ctx, r.APIReader, &route, parent.gateway)
	if err != nil {
		var buildError *routeBuildError
		if errors.As(err, &buildError) || apierrors.IsNotFound(err) {
			return snapshot, nil
		}
		return routeCleanupSnapshot{}, err
	}
	if desired.Identity == stored {
		snapshot.disposition = routeCleanupStillDesired
	}
	return snapshot, nil
}

func (r *Reconciler) observeRouteParentsForCleanup(
	ctx context.Context,
	route *gatewayv1.HTTPRoute,
) ([]routeParentObservation, error) {
	parents := make([]routeParentObservation, 0, len(route.Spec.ParentRefs))
	for _, parentRef := range route.Spec.ParentRefs {
		if !controller.IsGatewayParentRef(parentRef) {
			continue
		}
		observation := routeParentObservation{ref: parentRef}
		namespace := route.Namespace
		if parentRef.Namespace != nil {
			namespace = string(*parentRef.Namespace)
		}
		var gateway gatewayv1.Gateway
		if err := r.APIReader.Get(
			ctx,
			types.NamespacedName{Namespace: namespace, Name: string(parentRef.Name)},
			&gateway,
		); err != nil {
			if apierrors.IsNotFound(err) {
				parents = append(parents, observation)
				continue
			}
			return nil, fmt.Errorf("get parent Gateway during Gateway API version check: %w", err)
		}
		observation.gateway = &gateway
		var gatewayClass gatewayv1.GatewayClass
		if err := r.APIReader.Get(
			ctx,
			types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)},
			&gatewayClass,
		); err != nil {
			if apierrors.IsNotFound(err) {
				parents = append(parents, observation)
				continue
			}
			return nil, fmt.Errorf("get parent GatewayClass during Gateway API version check: %w", err)
		}
		observation.gatewayClass = &gatewayClass
		if gatewayClass.Spec.ControllerName == r.Config.ControllerName {
			observation.managed = true
			if len(gateway.Spec.Listeners) == 1 {
				observation.listener = &gateway.Spec.Listeners[0]
			}
		}
		parents = append(parents, observation)
	}
	return parents, nil
}

func routeCleanupAllowedDuringVersionMismatch(
	parents []routeParentObservation,
	stored cloud.Identity,
) bool {
	for _, parent := range parents {
		if parent.gateway == nil {
			continue
		}
		if parent.gatewayClass == nil {
			return false
		}
		if !parent.managed {
			continue
		}
		boundGatewayIsDeleting := parent.gateway.Namespace == stored.GatewayNamespace &&
			parent.gateway.Name == stored.GatewayName && string(parent.gateway.UID) == stored.GatewayUID &&
			!parent.gateway.DeletionTimestamp.IsZero()
		if !boundGatewayIsDeleting {
			return false
		}
	}
	return true
}

func managedParentsFromObservations(observations []routeParentObservation) []managedRouteParent {
	parents := make([]managedRouteParent, 0, len(observations))
	for _, observation := range observations {
		if !observation.managed {
			continue
		}
		parents = append(parents, managedRouteParent{
			ref:          observation.ref,
			gateway:      observation.gateway,
			gatewayClass: observation.gatewayClass,
			listener:     observation.listener,
		})
	}
	return parents
}

func (r *Reconciler) sealRouteCleanupSnapshot(
	ctx context.Context,
	snapshot routeCleanupSnapshot,
) error {
	var route gatewayv1.HTTPRoute
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(snapshot.route), &route); err != nil {
		return errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if !sameHTTPRouteRevision(
		&route,
		snapshot.route.UID,
		snapshot.route.Generation,
		!snapshot.route.DeletionTimestamp.IsZero(),
	) || route.ResourceVersion != snapshot.route.ResourceVersion {
		return errHTTPRouteChanged
	}
	stored, present, err := r.storedRouteIdentity(&route)
	if err != nil {
		return err
	}
	if !present || stored != snapshot.identity {
		return errHTTPRouteChanged
	}
	return nil
}
