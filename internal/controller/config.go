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

// Package controller contains the Kubernetes-facing Gateway API reconcilers.
// OpenStack SDK types are confined to internal/cloud/openstack.
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
	ControllerName          gatewayv1.GatewayController
	ControllerVersion       string
	OpenStackProjectID      string
	ClusterID               string
	Provider                string
	VIPSubnetID             string
	ExternalNetworkID       string
	MemberSubnetID          string
	MemberMode              MemberMode
	NodeAddressType         corev1.NodeAddressType
	HealthPath              string
	OpenStackResyncInterval time.Duration
}

// Validate rejects configuration that could make ownership ambiguous.
func (c Config) Validate() error {
	if len(c.ControllerName) > 253 || !gatewayControllerNamePattern.MatchString(string(c.ControllerName)) {
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

// Controller-scoped metadata keys include a digest of the complete controller
// name. The DNS prefix alone is not unique: two controllers may legitimately
// share a domain while owning different GatewayClasses.
func (c Config) gatewayFinalizer() string {
	return c.domain() + "/gateway-finalizer-" + c.controllerKey()
}
func (c Config) routeFinalizer() string {
	return c.domain() + "/httproute-finalizer-" + c.controllerKey()
}

// routeBindingFinalizer anchors the mutable annotations needed to find an
// HTTPRoute's OpenStack graph. Kubernetes users can edit annotations, so a
// digest in a separate controller finalizer makes accidental or partial
// binding changes fail closed instead of authorizing a lookup with a forged
// tuple. It is not intended as a security boundary for principals that may
// also edit finalizers.
func (c Config) routeBindingFinalizer(clusterID, projectID, gatewayNamespace, gatewayName, gatewayUID string) string {
	input := strings.Join([]string{clusterID, projectID, gatewayNamespace, gatewayName, gatewayUID}, "\x00")
	digest := sha256.Sum256([]byte(input))
	return c.routeBindingFinalizerPrefix() + fmt.Sprintf("%x", digest[:6])
}

func (c Config) routeBindingFinalizerPrefix() string {
	return c.domain() + "/httproute-binding-" + c.controllerKey() + "-"
}

func (c Config) domain() string { return strings.SplitN(string(c.ControllerName), "/", 2)[0] }
func (c Config) routeGatewayNamespaceAnnotation() string {
	return c.domain() + "/route-gateway-namespace-" + c.controllerKey()
}
func (c Config) routeGatewayNameAnnotation() string {
	return c.domain() + "/route-gateway-name-" + c.controllerKey()
}
func (c Config) routeGatewayUIDAnnotation() string {
	return c.domain() + "/route-gateway-uid-" + c.controllerKey()
}
func (c Config) gatewayListenerPortAnnotation() string {
	return c.domain() + "/gateway-listener-port-" + c.controllerKey()
}
func (c Config) gatewayClusterIDAnnotation() string {
	return c.domain() + "/gateway-cluster-id-" + c.controllerKey()
}
func (c Config) gatewayProjectIDAnnotation() string {
	return c.domain() + "/gateway-project-id-" + c.controllerKey()
}
func (c Config) routeClusterIDAnnotation() string {
	return c.domain() + "/route-cluster-id-" + c.controllerKey()
}
func (c Config) routeProjectIDAnnotation() string {
	return c.domain() + "/route-project-id-" + c.controllerKey()
}

// routeCleanupFailureAnnotation stores a reason that contains no sensitive data
// when Gateway API status has no valid parent entry for a cleanup diagnostic.
// It suppresses duplicate Events and is not part of the OpenStack ownership
// identity.
func (c Config) routeCleanupFailureAnnotation() string {
	return c.domain() + "/httproute-cleanup-failure-" + c.controllerKey()
}

func (c Config) controllerKey() string {
	digest := sha256.Sum256([]byte(c.ControllerName))
	return fmt.Sprintf("%x", digest[:6])
}
