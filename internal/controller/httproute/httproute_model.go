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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
)

type routeErrorKind string

const (
	routeErrorUnsupported         routeErrorKind = "Unsupported"
	routeErrorInvalidKind         routeErrorKind = "InvalidKind"
	routeErrorRefNotPermitted     routeErrorKind = "RefNotPermitted"
	routeErrorUnsupportedProtocol routeErrorKind = "UnsupportedProtocol"
	routeErrorNoMatchingParent    routeErrorKind = "NoMatchingParent"
	routeErrorNotAllowed          routeErrorKind = "NotAllowedByListeners"
	routeErrorPending             routeErrorKind = "Pending"
)

type routeBuildError struct {
	kind    routeErrorKind
	message string
}

func (e *routeBuildError) Error() string { return e.message }

func newRouteBuildError(kind routeErrorKind, format string, args ...any) error {
	return &routeBuildError{kind: kind, message: fmt.Sprintf(format, args...)}
}
func structurallySupportedRoute(route *gatewayv1.HTTPRoute) bool {
	if len(route.Spec.ParentRefs) != 1 || len(route.Spec.Hostnames) > 1 ||
		len(route.Spec.Hostnames) == 1 && strings.HasPrefix(string(route.Spec.Hostnames[0]), "*.") ||
		len(route.Spec.Rules) != 1 {
		return false
	}
	rule := route.Spec.Rules[0]
	if rule.Name != nil || len(rule.Filters) != 0 || rule.Timeouts != nil || rule.Retry != nil || rule.SessionPersistence != nil || len(rule.BackendRefs) != 1 {
		return false
	}
	if _, _, _, err := supportedMatch(rule.Matches); err != nil {
		return false
	}
	backend := rule.BackendRefs[0]
	backendNamespace := route.Namespace
	if backend.Namespace != nil {
		backendNamespace = string(*backend.Namespace)
	}
	return len(backend.Filters) == 0 && (backend.Weight == nil || *backend.Weight != 0) &&
		(backend.Group == nil || *backend.Group == gatewayv1.Group("")) &&
		(backend.Kind == nil || *backend.Kind == gatewayv1.Kind("Service")) &&
		backendNamespace == route.Namespace && backend.Port != nil
}

func (r *Reconciler) buildRouteSpecWithReader(
	ctx context.Context,
	reader client.Reader,
	route *gatewayv1.HTTPRoute,
	gateway *gatewayv1.Gateway,
) (cloud.RouteSpec, error) {
	if len(route.Spec.Hostnames) > 1 {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorUnsupported, "Phase 1 supports at most one exact hostname")
	}
	hostname := ""
	if len(route.Spec.Hostnames) == 1 {
		hostname = string(route.Spec.Hostnames[0])
		if strings.HasPrefix(hostname, "*.") {
			return cloud.RouteSpec{}, newRouteBuildError(routeErrorUnsupported, "wildcard hostnames are not supported in Phase 1")
		}
	}
	if len(route.Spec.Rules) != 1 {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorUnsupported, "Phase 1 supports exactly one HTTPRoute rule")
	}
	rule := route.Spec.Rules[0]
	if rule.Name != nil || len(rule.Filters) != 0 || rule.Timeouts != nil || rule.Retry != nil || rule.SessionPersistence != nil {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorUnsupported, "named rules, filters, timeouts, retries, and session persistence are not supported in Phase 1")
	}
	_, pathType, pathValue, err := supportedMatch(rule.Matches)
	if err != nil {
		return cloud.RouteSpec{}, err
	}
	if len(rule.BackendRefs) != 1 {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorUnsupported, "Phase 1 supports exactly one backendRef")
	}
	backend := rule.BackendRefs[0]
	if len(backend.Filters) != 0 {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorUnsupported, "backendRef filters are not supported in Phase 1")
	}
	if backend.Weight != nil && *backend.Weight == 0 {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorUnsupported, "a single Phase 1 backendRef must have non-zero weight")
	}
	if backend.Group != nil && *backend.Group != gatewayv1.Group("") {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorInvalidKind, "backendRef group %q is unsupported; use a core Service", *backend.Group)
	}
	if backend.Kind != nil && *backend.Kind != gatewayv1.Kind("Service") {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorInvalidKind, "backendRef kind %q is unsupported; use Service", *backend.Kind)
	}
	backendNamespace := route.Namespace
	if backend.Namespace != nil {
		backendNamespace = string(*backend.Namespace)
	}
	if backendNamespace != route.Namespace {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorRefNotPermitted, "cross-namespace backendRefs require ReferenceGrant and are deferred until Phase 5")
	}
	if backend.Port == nil {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorUnsupported, "backendRef port is required")
	}

	var service corev1.Service
	if err := reader.Get(ctx, types.NamespacedName{Namespace: backendNamespace, Name: string(backend.Name)}, &service); err != nil {
		return cloud.RouteSpec{}, fmt.Errorf("get backend Service: %w", err)
	}
	if service.Spec.Type != corev1.ServiceTypeNodePort {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorUnsupported, "backend Service %s must be type NodePort in Phase 1", service.Name)
	}
	servicePort, err := nodePortForBackend(&service, int32(*backend.Port))
	if err != nil {
		return cloud.RouteSpec{}, err
	}
	members, err := r.nodePortMembersWithReader(ctx, reader, &service, servicePort.NodePort)
	if err != nil {
		return cloud.RouteSpec{}, err
	}
	return cloud.RouteSpec{
		Identity: controller.RouteIdentity(r.Config, gateway, route),
		GatewayRequirements: cloud.GatewayRequirements{
			Provider:          r.Config.Provider,
			VIPSubnetID:       r.Config.VIPSubnetID,
			ExternalNetworkID: r.Config.ExternalNetworkID,
			ListenerPort:      int(gateway.Spec.Listeners[0].Port),
		},
		MemberSubnetID: r.Config.MemberSubnetID,
		HealthPath:     r.Config.HealthPath,
		Hostname:       hostname,
		PathType:       pathType,
		PathValue:      pathValue,
		Members:        members,
	}, nil
}

func supportedMatch(matches []gatewayv1.HTTPRouteMatch) (*gatewayv1.HTTPRouteMatch, cloud.PathMatchType, string, error) {
	if len(matches) == 0 {
		return nil, cloud.PathMatchPrefix, "/", nil
	}
	if len(matches) != 1 {
		return nil, "", "", newRouteBuildError(routeErrorUnsupported, "Phase 1 supports at most one HTTPRoute match")
	}
	match := &matches[0]
	if len(match.Headers) != 0 || len(match.QueryParams) != 0 || match.Method != nil {
		return nil, "", "", newRouteBuildError(routeErrorUnsupported, "header, query parameter, and method matches are not supported in Phase 1")
	}
	pathType := gatewayv1.PathMatchPathPrefix
	pathValue := "/"
	if match.Path != nil {
		if match.Path.Type != nil {
			pathType = *match.Path.Type
		}
		if match.Path.Value != nil {
			pathValue = *match.Path.Value
		}
	}
	if !validHTTPPath(pathValue) {
		return nil, "", "", newRouteBuildError(routeErrorUnsupported, "path %q is not a valid absolute Gateway API path", pathValue)
	}
	switch pathType {
	case gatewayv1.PathMatchExact:
		return match, cloud.PathMatchExact, pathValue, nil
	case gatewayv1.PathMatchPathPrefix:
		return match, cloud.PathMatchPrefix, pathValue, nil
	default:
		return nil, "", "", newRouteBuildError(routeErrorUnsupported, "path match type %q is not supported", pathType)
	}
}

func validHTTPPath(value string) bool {
	if !strings.HasPrefix(value, "/") || strings.Contains(value, "//") || strings.Contains(value, "/./") || strings.Contains(value, "/../") {
		return false
	}
	return value != "/." && value != "/.." && !strings.HasSuffix(value, "/.") && !strings.HasSuffix(value, "/..")
}
