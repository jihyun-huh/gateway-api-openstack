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
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller/graph"
)

func (r *Reconciler) detachPreviousGateway(
	ctx context.Context,
	route *gatewayv1.HTTPRoute,
	gateway *gatewayv1.Gateway,
) (ctrl.Result, error) {
	routeUID, generation, isDeleting := route.UID, route.Generation, !route.DeletionTimestamp.IsZero()
	current := &gatewayv1.HTTPRoute{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(route), current); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !sameHTTPRouteRevision(current, routeUID, generation, isDeleting) {
		return ctrl.Result{}, errHTTPRouteChanged
	}
	*route = *current

	stored, present, err := r.storedRouteIdentity(current)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !present {
		if controllerutil.ContainsFinalizer(current, r.Config.RouteFinalizer()) {
			return ctrl.Result{}, fmt.Errorf("HTTPRoute %s/%s has the controller finalizer but no complete stored Gateway identity", current.Namespace, current.Name)
		}
		return ctrl.Result{}, nil
	}
	if stored.GatewayNamespace == gateway.Namespace && stored.GatewayName == gateway.Name && stored.GatewayUID == string(gateway.UID) {
		return ctrl.Result{}, nil
	}
	return r.detachRoute(ctx, route)
}

func (r *Reconciler) detachRoute(ctx context.Context, route *gatewayv1.HTTPRoute) (ctrl.Result, error) {
	routeKey := client.ObjectKeyFromObject(route)
	routeUID, generation, isDeleting := route.UID, route.Generation, !route.DeletionTimestamp.IsZero()
	current, stored, present, err := r.getRouteBinding(ctx, routeKey, routeUID, generation, isDeleting)
	if err != nil {
		return ctrl.Result{}, err
	}
	if current == nil {
		return ctrl.Result{}, nil
	}
	if !present && controllerutil.ContainsFinalizer(current, r.Config.RouteFinalizer()) {
		return ctrl.Result{}, fmt.Errorf("HTTPRoute %s has the controller finalizer but no complete stored Gateway identity", routeKey)
	}
	if present {
		outcome, err := r.deleteRoute(ctx, current, stored)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("delete detached HTTPRoute resources: %w", err)
		}
		if err := outcome.Validate(); err != nil {
			return ctrl.Result{}, fmt.Errorf("validate detached HTTPRoute deletion outcome: %w", err)
		}
		if outcome.State == cloud.OutcomeProgressing {
			if err := r.clearRouteCleanupFailure(ctx, current); err != nil {
				return ctrl.Result{}, fmt.Errorf("clear HTTPRoute cleanup failure checkpoint: %w", err)
			}
			*route = *current
			return ctrl.Result{RequeueAfter: controller.ProviderProgressRequeueAfter(outcome, current.UID)}, nil
		}
		log.FromContext(ctx).V(1).Info("Deleted detached HTTPRoute resources", "gateway", types.NamespacedName{Namespace: stored.GatewayNamespace, Name: stored.GatewayName})
	}
	if err := r.clearStoredRouteBinding(ctx, route, stored, present); err != nil {
		return ctrl.Result{}, err
	}
	log.FromContext(ctx).V(1).Info("Detached HTTPRoute from Gateway")
	return ctrl.Result{}, nil
}

func (r *Reconciler) finalizeRoute(ctx context.Context, scope *httpRouteScope) (ctrl.Result, error) {
	route := scope.route
	routeKey := client.ObjectKeyFromObject(route)
	current, stored, present, err := r.getRouteBinding(
		ctx,
		routeKey,
		route.UID,
		route.Generation,
		!route.DeletionTimestamp.IsZero(),
	)
	if err != nil {
		return ctrl.Result{}, err
	}
	if current == nil {
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(current, r.Config.RouteFinalizer()) && !present {
		return ctrl.Result{}, nil
	}
	if !present {
		return ctrl.Result{}, fmt.Errorf("HTTPRoute %s has the controller finalizer but no complete stored Gateway identity", routeKey)
	}
	outcome, err := r.deleteRoute(ctx, current, stored)
	if err != nil {
		providerErr := fmt.Errorf("delete HTTPRoute resources during finalization: %w", err)
		*route = *current
		return r.handleRouteFinalizationFailure(ctx, scope, stored, providerErr)
	}
	if err := outcome.Validate(); err != nil {
		return ctrl.Result{}, fmt.Errorf("validate HTTPRoute finalization outcome: %w", err)
	}
	if outcome.State == cloud.OutcomeProgressing {
		if err := r.clearRouteCleanupFailure(ctx, current); err != nil {
			return ctrl.Result{}, fmt.Errorf("clear HTTPRoute cleanup failure checkpoint: %w", err)
		}
		*route = *current
		return ctrl.Result{RequeueAfter: controller.ProviderProgressRequeueAfter(outcome, current.UID)}, nil
	}
	log.FromContext(ctx).V(1).Info("Deleted HTTPRoute resources during finalization", "gateway", types.NamespacedName{Namespace: stored.GatewayNamespace, Name: stored.GatewayName})
	if err := r.clearStoredRouteBinding(ctx, route, stored, true); err != nil {
		return ctrl.Result{}, err
	}
	log.FromContext(ctx).V(1).Info("Removed HTTPRoute finalizer")
	return ctrl.Result{}, nil
}

func (r *Reconciler) setRouteCleanupFailure(
	ctx context.Context,
	route *gatewayv1.HTTPRoute,
	reason string,
) (bool, error) {
	return r.patchRouteCleanupFailure(ctx, route, reason)
}

func (r *Reconciler) clearRouteCleanupFailure(ctx context.Context, route *gatewayv1.HTTPRoute) error {
	_, err := r.patchRouteCleanupFailure(ctx, route, "")
	return err
}

func (r *Reconciler) patchRouteCleanupFailure(
	ctx context.Context,
	route *gatewayv1.HTTPRoute,
	reason string,
) (bool, error) {
	routeKey := client.ObjectKeyFromObject(route)
	routeUID, generation, isDeleting := route.UID, route.Generation, !route.DeletionTimestamp.IsZero()
	changed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		changed = false
		current := &gatewayv1.HTTPRoute{}
		if err := r.routePatchReader().Get(ctx, routeKey, current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !sameHTTPRouteRevision(current, routeUID, generation, isDeleting) {
			return errHTTPRouteChanged
		}
		key := r.Config.RouteCleanupFailureAnnotation()
		if current.Annotations[key] == reason {
			*route = *current
			return nil
		}

		base := current.DeepCopy()
		if reason == "" {
			delete(current.Annotations, key)
		} else {
			if current.Annotations == nil {
				current.Annotations = map[string]string{}
			}
			current.Annotations[key] = reason
		}
		if err := r.Patch(ctx, current, controller.OptimisticMergeFrom(base)); err != nil {
			return client.IgnoreNotFound(err)
		}
		changed = true
		*route = *current
		return nil
	})
	return changed, err
}

func (r *Reconciler) deleteRoute(
	ctx context.Context,
	expected *gatewayv1.HTTPRoute,
	identity cloud.Identity,
) (cloud.Outcome, error) {
	if r.APIReader == nil {
		return cloud.Outcome{}, controller.ErrAPIReaderRequired
	}
	release, err := graph.Acquire(ctx, r.Coordinator, identity.GatewayUID)
	if err != nil {
		return cloud.Outcome{}, fmt.Errorf("acquire Gateway graph: %w", err)
	}
	defer release()

	snapshot, err := r.buildRouteCleanupSnapshot(ctx, expected, identity)
	if err != nil {
		return cloud.Outcome{}, err
	}
	switch snapshot.disposition {
	case routeCleanupStillDesired:
		return cloud.Outcome{}, errHTTPRouteChanged
	case routeCleanupRequired:
	default:
		return cloud.Outcome{}, fmt.Errorf("invalid HTTPRoute cleanup snapshot disposition %d", snapshot.disposition)
	}
	if err := r.sealRouteCleanupSnapshot(ctx, snapshot); err != nil {
		return cloud.Outcome{}, err
	}

	return r.Provider.DeleteRoute(ctx, snapshot.identity)
}
