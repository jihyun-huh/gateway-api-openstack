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

// Package cloud defines the provider-neutral boundary used by the Kubernetes
// reconcilers. Provider SDK types must not cross this package boundary.
package cloud

import (
	"context"
	"fmt"
)

// Identity is the immutable ownership anchor for a Gateway and its resources.
// Route fields are set only for resources whose lifecycle is owned by one
// HTTPRoute.
type Identity struct {
	OpenStackProjectID string `json:"openStackProjectID,omitempty"`
	ClusterID          string `json:"clusterID"`
	Controller         string `json:"controller"`
	ControllerVersion  string `json:"controllerVersion,omitempty"`
	GatewayNamespace   string `json:"gatewayNamespace"`
	GatewayName        string `json:"gatewayName"`
	GatewayUID         string `json:"gatewayUID"`
	RouteNamespace     string `json:"routeNamespace,omitempty"`
	RouteName          string `json:"routeName,omitempty"`
	RouteUID           string `json:"routeUID,omitempty"`
}

// GatewaySpec describes the Phase 1 OpenStack resources owned by a Gateway.
type GatewaySpec struct {
	Identity          Identity
	Provider          string
	VIPSubnetID       string
	ExternalNetworkID string
	ListenerName      string
	ListenerPort      int
}

// GatewayState is the provider-neutral result published to Gateway status and
// passed to route reconciliation.
type GatewayState struct {
	LoadBalancerID    string
	VIPPortID         string
	VIPAddress        string
	FloatingIPID      string
	FloatingIPAddress string
	ListenerID        string
}

// GatewayRequirements contains the immutable Gateway fields and the settings
// that route reconciliation must verify before it can restore traffic.
type GatewayRequirements struct {
	Provider          string
	VIPSubnetID       string
	ExternalNetworkID string
	ListenerPort      int
}

// Address returns the client-facing address, preferring a Floating IP when
// one is configured.
func (s GatewayState) Address() string {
	if s.FloatingIPAddress != "" {
		return s.FloatingIPAddress
	}
	return s.VIPAddress
}

// PathMatchType is the small HTTP path subset supported in Phase 1.
type PathMatchType string

const (
	// PathMatchExact maps to an Octavia EQUAL_TO path rule.
	PathMatchExact PathMatchType = "Exact"
	// PathMatchPrefix maps to an exact policy plus a segment-prefix policy so
	// /foo does not accidentally match /foobar.
	PathMatchPrefix PathMatchType = "PathPrefix"
)

// Member is one Octavia backend member.
type Member struct {
	Address string
	Port    int
}

// RouteSpec describes the Phase 1 pool and L7 policies owned by one route.
type RouteSpec struct {
	Identity            Identity
	Gateway             GatewayState
	GatewayRequirements GatewayRequirements
	MemberSubnetID      string
	HealthPath          string
	Hostname            string
	PathType            PathMatchType
	PathValue           string
	Members             []Member
}

// RouteState records the OpenStack objects produced for one HTTPRoute.
type RouteState struct {
	PoolID      string
	MemberIDs   []string
	MonitorID   string
	L7PolicyIDs []string
	L7RuleIDs   []string
}

// Provider is the complete cloud boundary used by the Phase 1 reconcilers.
// Ensure operations must be idempotent, and Delete operations must verify the
// complete identity before removing anything.
type Provider interface {
	EnsureGateway(context.Context, GatewaySpec) (GatewayResult, error)
	GetGateway(context.Context, Identity) (GatewayResult, bool, error)
	DeleteGateway(context.Context, Identity) (Outcome, error)
	EnsureRoute(context.Context, RouteSpec) (RouteResult, error)
	DeleteRoute(context.Context, Identity) (Outcome, error)
}

// ValidateGatewayIdentity verifies the common Gateway ownership tuple.
func ValidateGatewayIdentity(id Identity) error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "cluster ID", value: id.ClusterID},
		{name: "controller", value: id.Controller},
		{name: "Gateway namespace", value: id.GatewayNamespace},
		{name: "Gateway name", value: id.GatewayName},
		{name: "Gateway UID", value: id.GatewayUID},
	} {
		if field.value == "" {
			return fmt.Errorf("%s must not be empty", field.name)
		}
	}
	return nil
}

// ValidateRouteIdentity verifies both the Gateway and HTTPRoute ownership
// tuples.
func ValidateRouteIdentity(id Identity) error {
	if err := ValidateGatewayIdentity(id); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "HTTPRoute namespace", value: id.RouteNamespace},
		{name: "HTTPRoute name", value: id.RouteName},
		{name: "HTTPRoute UID", value: id.RouteUID},
	} {
		if field.value == "" {
			return fmt.Errorf("%s must not be empty", field.name)
		}
	}
	return nil
}
