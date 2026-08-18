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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

type gatewayValidationError struct {
	message           string
	invalidRouteKinds bool
	gatewayReason     gatewayv1.GatewayConditionReason
}

func (e *gatewayValidationError) Error() string { return e.message }

func (r *GatewayReconciler) getGatewayClass(ctx context.Context, gateway *gatewayv1.Gateway) (*gatewayv1.GatewayClass, bool, error) {
	var gatewayClass gatewayv1.GatewayClass
	key := types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)}
	if err := r.Get(ctx, key, &gatewayClass); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get GatewayClass %q: %w", key.Name, err)
	}
	return &gatewayClass, gatewayClass.Spec.ControllerName == r.Config.ControllerName, nil
}

func validateGateway(gateway *gatewayv1.Gateway) (*gatewayv1.Listener, error) {
	if len(gateway.Spec.Addresses) != 0 {
		return nil, &gatewayValidationError{
			message:       "spec.addresses is not supported in Phase 1",
			gatewayReason: gatewayv1.GatewayReasonUnsupportedAddress,
		}
	}
	if gateway.Spec.Infrastructure != nil {
		return nil, &gatewayValidationError{
			message:       "spec.infrastructure is not supported in Phase 1",
			gatewayReason: gatewayv1.GatewayReasonInvalidParameters,
		}
	}
	if gateway.Spec.AllowedListeners != nil {
		return nil, &gatewayValidationError{
			message:       "spec.allowedListeners and ListenerSet attachment are not supported in Phase 1",
			gatewayReason: gatewayv1.GatewayReasonInvalidParameters,
		}
	}
	if gateway.Spec.TLS != nil {
		return nil, &gatewayValidationError{
			message:       "gateway-wide spec.tls is not supported in Phase 1",
			gatewayReason: gatewayv1.GatewayReasonInvalidParameters,
		}
	}
	if len(gateway.Spec.Listeners) != 1 {
		return nil, fmt.Errorf("exactly one listener is supported in Phase 1")
	}
	listener := &gateway.Spec.Listeners[0]
	if listener.Protocol != gatewayv1.HTTPProtocolType || listener.TLS != nil {
		return nil, fmt.Errorf("only a plain HTTP listener is supported in Phase 1")
	}
	if listener.Hostname != nil {
		return nil, fmt.Errorf("listener hostname is not supported in Phase 1; put one exact hostname on HTTPRoute")
	}
	if listener.AllowedRoutes != nil && listener.AllowedRoutes.Namespaces != nil &&
		listener.AllowedRoutes.Namespaces.From != nil &&
		*listener.AllowedRoutes.Namespaces.From != gatewayv1.NamespacesFromSame {
		return nil, fmt.Errorf("only same-namespace HTTPRoutes are supported in Phase 1")
	}
	if listener.AllowedRoutes != nil && len(listener.AllowedRoutes.Kinds) != 0 {
		for _, kind := range listener.AllowedRoutes.Kinds {
			if !isHTTPRouteGroupKind(kind) {
				return nil, &gatewayValidationError{
					message:           "only HTTPRoute is supported in listener allowedRoutes.kinds in Phase 1",
					invalidRouteKinds: true,
				}
			}
		}
	}
	return listener, nil
}

func listenerAllowsHTTPRoute(listener gatewayv1.Listener) bool {
	if listener.AllowedRoutes == nil || len(listener.AllowedRoutes.Kinds) == 0 {
		return true
	}
	for _, kind := range listener.AllowedRoutes.Kinds {
		if isHTTPRouteGroupKind(kind) {
			return true
		}
	}
	return false
}

func isHTTPRouteGroupKind(routeKind gatewayv1.RouteGroupKind) bool {
	group := gatewayv1.Group(gatewayv1.GroupName)
	if routeKind.Group != nil {
		group = *routeKind.Group
	}
	return group == gatewayv1.Group(gatewayv1.GroupName) && routeKind.Kind == gatewayv1.Kind("HTTPRoute")
}
