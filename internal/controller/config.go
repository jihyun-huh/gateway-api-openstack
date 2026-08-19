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

// Package controller contains Kubernetes-facing contracts shared by the
// Gateway API reconciler packages. OpenStack SDK types are confined to
// internal/cloud/openstack.
package controller

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// MemberMode selects how Kubernetes backends become Octavia members.
type MemberMode string

const (
	// MemberModeNodePort sends Octavia traffic to ready Kubernetes Nodes and a
	// Service NodePort. It is the only backend mode supported in Phase 1.
	MemberModeNodePort MemberMode = "NodePort"

	// DefaultOpenStackResyncInterval controls how often a converged resource is
	// observed when no Kubernetes event has changed its desired state.
	DefaultOpenStackResyncInterval = 10 * time.Minute
)

var gatewayControllerNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*\/[A-Za-z0-9/\-._~%!$&'()*+,;=:]+$`)

// Config is the cluster-wide controller configuration.
type Config struct {
	// ControllerName is the exact Gateway API ownership identifier.
	ControllerName gatewayv1.GatewayController
	// ControllerVersion is recorded as trace metadata on cloud resources.
	ControllerVersion string
	// OpenStackProjectID is the authenticated project used in ownership checks.
	OpenStackProjectID string
	// ClusterID is the stable cluster identity stored on managed resources.
	ClusterID string
	// Provider is the Octavia provider selected for managed load balancers.
	Provider string
	// VIPSubnetID is the subnet used for Octavia virtual IPs.
	VIPSubnetID string
	// ExternalNetworkID is the optional network used for Floating IPs.
	ExternalNetworkID string
	// MemberSubnetID is the subnet associated with Octavia members.
	MemberSubnetID string
	// MemberMode selects how Service backends become Octavia members.
	MemberMode MemberMode
	// NodeAddressType selects the Node address used by NodePort members.
	NodeAddressType corev1.NodeAddressType
	// HealthPath is the HTTP path used by Octavia health monitors.
	HealthPath string
	// OpenStackResyncInterval controls periodic observation after convergence.
	OpenStackResyncInterval time.Duration
}

// Validate rejects configuration that could make ownership ambiguous.
func (c Config) Validate() error {
	if !IsValidControllerName(c.ControllerName) {
		return fmt.Errorf("controller name %q must be a domain prefixed path", c.ControllerName)
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "cluster ID", value: c.ClusterID},
		{name: "octavia provider", value: c.Provider},
		{name: "VIP subnet ID", value: c.VIPSubnetID},
		{name: "member subnet ID", value: c.MemberSubnetID},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s must not be empty", field.name)
		}
	}
	if c.Provider != "amphora" {
		return fmt.Errorf("octavia provider %q is unsupported in Phase 1; only amphora has been verified", c.Provider)
	}
	if c.MemberMode != MemberModeNodePort {
		return fmt.Errorf("member mode %q is unsupported in Phase 1; use %s", c.MemberMode, MemberModeNodePort)
	}
	if c.NodeAddressType != corev1.NodeInternalIP && c.NodeAddressType != corev1.NodeExternalIP {
		return fmt.Errorf("node address type must be InternalIP or ExternalIP")
	}
	if !strings.HasPrefix(c.HealthPath, "/") {
		return fmt.Errorf("health path must begin with '/'")
	}
	if c.OpenStackResyncInterval <= 0 {
		return fmt.Errorf("OpenStack resync interval must be greater than zero")
	}
	return nil
}

// IsValidControllerName reports whether a Gateway controller name can be used
// to derive controller-scoped metadata keys.
func IsValidControllerName(name gatewayv1.GatewayController) bool {
	return len(name) <= 253 && gatewayControllerNamePattern.MatchString(string(name))
}

// GatewayFinalizer returns the controller-scoped Gateway finalizer. The key
// includes a digest because the DNS prefix alone does not identify a
// controller name uniquely.
func (c Config) GatewayFinalizer() string {
	return c.Domain() + "/gateway-finalizer-" + c.controllerKey()
}

// RouteFinalizer returns the controller-scoped HTTPRoute finalizer.
func (c Config) RouteFinalizer() string {
	return c.Domain() + "/httproute-finalizer-" + c.controllerKey()
}

// RouteBindingFinalizer returns the finalizer that anchors the mutable
// annotations needed to find an
// HTTPRoute's OpenStack graph. Kubernetes users can edit annotations, so a
// digest in a separate controller finalizer makes accidental or partial
// binding changes fail closed instead of authorizing a lookup with a forged
// tuple. It is not intended as a security boundary for principals that may
// also edit finalizers.
func (c Config) RouteBindingFinalizer(clusterID, projectID, gatewayNamespace, gatewayName, gatewayUID string) string {
	input := strings.Join([]string{clusterID, projectID, gatewayNamespace, gatewayName, gatewayUID}, "\x00")
	digest := sha256.Sum256([]byte(input))
	return c.RouteBindingFinalizerPrefix() + fmt.Sprintf("%x", digest[:6])
}

// RouteBindingFinalizerPrefix returns the controller-scoped prefix shared by
// HTTPRoute binding finalizers.
func (c Config) RouteBindingFinalizerPrefix() string {
	return c.Domain() + "/httproute-binding-" + c.controllerKey() + "-"
}

// Domain returns the DNS prefix of the configured Gateway controller name.
func (c Config) Domain() string { return strings.SplitN(string(c.ControllerName), "/", 2)[0] }

// RouteGatewayNamespaceAnnotation returns the key that records the bound
// Gateway namespace on an HTTPRoute.
func (c Config) RouteGatewayNamespaceAnnotation() string {
	return c.Domain() + "/route-gateway-namespace-" + c.controllerKey()
}

// RouteGatewayNameAnnotation returns the key that records the bound Gateway
// name on an HTTPRoute.
func (c Config) RouteGatewayNameAnnotation() string {
	return c.Domain() + "/route-gateway-name-" + c.controllerKey()
}

// RouteGatewayUIDAnnotation returns the key that records the bound Gateway UID
// on an HTTPRoute.
func (c Config) RouteGatewayUIDAnnotation() string {
	return c.Domain() + "/route-gateway-uid-" + c.controllerKey()
}

// GatewayListenerPortAnnotation returns the key that records the listener port
// in a Gateway binding.
func (c Config) GatewayListenerPortAnnotation() string {
	return c.Domain() + "/gateway-listener-port-" + c.controllerKey()
}

// GatewayClusterIDAnnotation returns the key that records the cluster ID in a
// Gateway binding.
func (c Config) GatewayClusterIDAnnotation() string {
	return c.Domain() + "/gateway-cluster-id-" + c.controllerKey()
}

// GatewayProjectIDAnnotation returns the key that records the OpenStack
// project ID in a Gateway binding.
func (c Config) GatewayProjectIDAnnotation() string {
	return c.Domain() + "/gateway-project-id-" + c.controllerKey()
}

// RouteClusterIDAnnotation returns the key that records the cluster ID in an
// HTTPRoute binding.
func (c Config) RouteClusterIDAnnotation() string {
	return c.Domain() + "/route-cluster-id-" + c.controllerKey()
}

// RouteProjectIDAnnotation returns the key that records the OpenStack project
// ID in an HTTPRoute binding.
func (c Config) RouteProjectIDAnnotation() string {
	return c.Domain() + "/route-project-id-" + c.controllerKey()
}

// RouteCleanupFailureAnnotation returns the key used to store a cleanup reason
// when Gateway API status has no valid parent entry for a cleanup diagnostic.
// The stored reason contains no sensitive data.
// It suppresses duplicate Events and is not part of the OpenStack ownership
// identity.
func (c Config) RouteCleanupFailureAnnotation() string {
	return c.Domain() + "/httproute-cleanup-failure-" + c.controllerKey()
}

func (c Config) controllerKey() string {
	digest := sha256.Sum256([]byte(c.ControllerName))
	return fmt.Sprintf("%x", digest[:6])
}
