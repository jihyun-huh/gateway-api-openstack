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
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

// ValidateGatewayBinding verifies the durable controller identity stored on a
// Gateway without performing Kubernetes or cloud I/O.
func ValidateGatewayBinding(config Config, gateway *gatewayv1.Gateway) error {
	bindingValues := []string{
		gateway.Annotations[config.GatewayListenerPortAnnotation()],
		gateway.Annotations[config.GatewayClusterIDAnnotation()],
		gateway.Annotations[config.GatewayProjectIDAnnotation()],
	}
	populatedBindingCount := 0
	for _, value := range bindingValues {
		if value != "" {
			populatedBindingCount++
		}
	}
	if populatedBindingCount != len(bindingValues) && (populatedBindingCount != 0 || controllerutil.ContainsFinalizer(gateway, config.GatewayFinalizer())) {
		return fmt.Errorf("gateway %s/%s has an incomplete stored OpenStack resource binding; restore the original annotations before reconciling", gateway.Namespace, gateway.Name)
	}
	if stored := gateway.Annotations[config.GatewayClusterIDAnnotation()]; stored != "" && stored != config.ClusterID {
		return fmt.Errorf("controller cluster identity differs from this Gateway's existing resource binding; restore the original cluster ID before reconciling")
	}
	if stored := gateway.Annotations[config.GatewayProjectIDAnnotation()]; stored != "" && stored != config.OpenStackProjectID {
		return fmt.Errorf("authenticated OpenStack project differs from this Gateway's existing resource binding; restore access to the original project before reconciling")
	}
	return nil
}

// GatewayHasControllerBinding reports whether the Gateway contains any part
// of this controller's durable binding.
func GatewayHasControllerBinding(config Config, gateway *gatewayv1.Gateway) bool {
	if controllerutil.ContainsFinalizer(gateway, config.GatewayFinalizer()) {
		return true
	}
	for _, key := range []string{
		config.GatewayListenerPortAnnotation(),
		config.GatewayClusterIDAnnotation(),
		config.GatewayProjectIDAnnotation(),
	} {
		if gateway.Annotations[key] != "" {
			return true
		}
	}
	return false
}

// GatewayBindingMetadataEqual compares only Gateway metadata owned by this
// controller.
func GatewayBindingMetadataEqual(config Config, left, right *gatewayv1.Gateway) bool {
	if controllerutil.ContainsFinalizer(left, config.GatewayFinalizer()) !=
		controllerutil.ContainsFinalizer(right, config.GatewayFinalizer()) {
		return false
	}
	for _, key := range []string{
		config.GatewayListenerPortAnnotation(),
		config.GatewayClusterIDAnnotation(),
		config.GatewayProjectIDAnnotation(),
	} {
		if left.Annotations[key] != right.Annotations[key] {
			return false
		}
	}
	return true
}

// StoredGatewayIdentity returns the Gateway identity using durable binding
// values when they are present.
func StoredGatewayIdentity(config Config, gateway *gatewayv1.Gateway) cloud.Identity {
	identity := GatewayIdentity(config, gateway)
	if value := gateway.Annotations[config.GatewayClusterIDAnnotation()]; value != "" {
		identity.ClusterID = value
	}
	if value := gateway.Annotations[config.GatewayProjectIDAnnotation()]; value != "" {
		identity.OpenStackProjectID = value
	}
	return identity
}

// SameGatewayIdentity compares the Gateway portion of two cloud identities.
func SameGatewayIdentity(left, right cloud.Identity) bool {
	return left.GatewayNamespace == right.GatewayNamespace &&
		left.GatewayName == right.GatewayName &&
		left.GatewayUID == right.GatewayUID
}

// StoredRouteIdentity validates and returns the Gateway identity recorded by
// an HTTPRoute binding.
func StoredRouteIdentity(config Config, route *gatewayv1.HTTPRoute) (cloud.Identity, bool, error) {
	annotations := route.Annotations
	values := []string{
		annotations[config.RouteGatewayNamespaceAnnotation()],
		annotations[config.RouteGatewayNameAnnotation()],
		annotations[config.RouteGatewayUIDAnnotation()],
	}
	empty := 0
	for _, value := range values {
		if value == "" {
			empty++
		}
	}
	if empty == len(values) {
		if annotations[config.RouteClusterIDAnnotation()] != "" || annotations[config.RouteProjectIDAnnotation()] != "" || hasRouteBindingFinalizer(config, route) {
			return cloud.Identity{}, false, fmt.Errorf("HTTPRoute %s/%s has an incomplete stored Gateway identity", route.Namespace, route.Name)
		}
		return cloud.Identity{}, false, nil
	}
	if empty != 0 {
		return cloud.Identity{}, false, fmt.Errorf("HTTPRoute %s/%s has an incomplete stored Gateway identity", route.Namespace, route.Name)
	}
	clusterID := annotations[config.RouteClusterIDAnnotation()]
	if clusterID == "" {
		clusterID = config.ClusterID
	}
	projectID := annotations[config.RouteProjectIDAnnotation()]
	if projectID == "" {
		projectID = config.OpenStackProjectID
	}
	identity := cloud.Identity{
		OpenStackProjectID: projectID,
		ClusterID:          clusterID,
		Controller:         string(config.ControllerName),
		ControllerVersion:  config.ControllerVersion,
		GatewayNamespace:   values[0],
		GatewayName:        values[1],
		GatewayUID:         values[2],
		RouteNamespace:     route.Namespace,
		RouteName:          route.Name,
		RouteUID:           string(route.UID),
	}
	if identity.ClusterID != config.ClusterID {
		return cloud.Identity{}, false, fmt.Errorf("controller cluster identity differs from HTTPRoute %s/%s's existing resource binding; restore the original cluster ID before reconciling", route.Namespace, route.Name)
	}
	if identity.OpenStackProjectID != config.OpenStackProjectID {
		return cloud.Identity{}, false, fmt.Errorf("authenticated OpenStack project differs from HTTPRoute %s/%s's existing resource binding; restore access to the original project before reconciling", route.Namespace, route.Name)
	}
	if err := cloud.ValidateRouteIdentity(identity); err != nil {
		return cloud.Identity{}, false, err
	}
	expectedBindingFinalizer := config.RouteBindingFinalizer(
		identity.ClusterID,
		identity.OpenStackProjectID,
		identity.GatewayNamespace,
		identity.GatewayName,
		identity.GatewayUID,
	)
	if controllerutil.ContainsFinalizer(route, config.RouteFinalizer()) && !controllerutil.ContainsFinalizer(route, expectedBindingFinalizer) {
		return cloud.Identity{}, false, fmt.Errorf("HTTPRoute %s/%s stored Gateway identity does not match its binding finalizer; restore the original binding before reconciling", route.Namespace, route.Name)
	}
	for _, finalizer := range route.Finalizers {
		if strings.HasPrefix(finalizer, config.RouteBindingFinalizerPrefix()) && finalizer != expectedBindingFinalizer {
			return cloud.Identity{}, false, fmt.Errorf("HTTPRoute %s/%s has a conflicting Gateway binding finalizer", route.Namespace, route.Name)
		}
	}
	return identity, true, nil
}

// hasRouteBindingFinalizer reports whether any finalizer anchors an HTTPRoute
// binding owned by this controller.
func hasRouteBindingFinalizer(config Config, route *gatewayv1.HTTPRoute) bool {
	for _, finalizer := range route.Finalizers {
		if strings.HasPrefix(finalizer, config.RouteBindingFinalizerPrefix()) {
			return true
		}
	}
	return false
}

// routeHasBindingAnnotations reports whether an HTTPRoute contains any stored
// Gateway identity annotation owned by this controller.
func routeHasBindingAnnotations(config Config, route *gatewayv1.HTTPRoute) bool {
	for _, key := range []string{
		config.RouteGatewayNamespaceAnnotation(),
		config.RouteGatewayNameAnnotation(),
		config.RouteGatewayUIDAnnotation(),
		config.RouteClusterIDAnnotation(),
		config.RouteProjectIDAnnotation(),
	} {
		if route.Annotations[key] != "" {
			return true
		}
	}
	return false
}

// RouteHasControllerBinding reports whether an HTTPRoute contains any part of
// this controller's durable binding.
func RouteHasControllerBinding(config Config, route *gatewayv1.HTTPRoute) bool {
	return controllerutil.ContainsFinalizer(route, config.RouteFinalizer()) ||
		hasRouteBindingFinalizer(config, route) ||
		routeHasBindingAnnotations(config, route)
}
