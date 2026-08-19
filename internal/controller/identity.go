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
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

// GatewayIdentity builds the immutable cloud identity for a Gateway.
func GatewayIdentity(cfg Config, gateway *gatewayv1.Gateway) cloud.Identity {
	return cloud.Identity{
		OpenStackProjectID: cfg.OpenStackProjectID,
		ClusterID:          cfg.ClusterID,
		Controller:         string(cfg.ControllerName),
		ControllerVersion:  cfg.ControllerVersion,
		GatewayNamespace:   gateway.Namespace,
		GatewayName:        gateway.Name,
		GatewayUID:         string(gateway.UID),
	}
}

// RouteIdentity builds the immutable cloud identity for resources owned by an
// HTTPRoute in a Gateway graph.
func RouteIdentity(cfg Config, gateway *gatewayv1.Gateway, route *gatewayv1.HTTPRoute) cloud.Identity {
	value := GatewayIdentity(cfg, gateway)
	value.RouteNamespace = route.Namespace
	value.RouteName = route.Name
	value.RouteUID = string(route.UID)
	return value
}
