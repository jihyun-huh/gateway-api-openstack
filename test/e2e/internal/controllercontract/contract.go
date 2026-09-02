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

// Package controllercontract defines the controller installation contract
// shared by the E2E harness and its run-scoped installer.
package controllercontract

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// RunAnnotation binds a Kubernetes object to one E2E run.
	RunAnnotation = "e2e.gateway-api-openstack.io/run-id"
	// ConfigSourceAnnotation binds a controller Pod template to an immutable ConfigMap API version.
	ConfigSourceAnnotation = "e2e.gateway-api-openstack.io/controller-config-source"
	// CloudsSourceAnnotation binds a controller Pod template to an immutable Secret API version.
	CloudsSourceAnnotation = "e2e.gateway-api-openstack.io/controller-clouds-source"

	// CloudsMountPath is the exact directory used for the controller clouds.yaml Secret.
	CloudsMountPath = "/etc/openstack"
	// MetricsPortName is the required controller metrics port name.
	MetricsPortName = "metrics"
	// MetricsPort is the required controller metrics port number.
	MetricsPort int32 = 8080

	// EnvControllerName selects the Gateway API controller identity.
	EnvControllerName = "GATEWAY_OPENSTACK_CONTROLLER_NAME"
	// EnvClusterID selects the stable cluster identity recorded on OpenStack resources.
	EnvClusterID = "GATEWAY_OPENSTACK_CLUSTER_ID"
	// EnvOctaviaProvider selects the supported Octavia provider.
	EnvOctaviaProvider = "GATEWAY_OPENSTACK_OCTAVIA_PROVIDER"
	// EnvVIPSubnetID selects the Gateway VIP subnet.
	EnvVIPSubnetID = "GATEWAY_OPENSTACK_VIP_SUBNET_ID"
	// EnvExternalNetworkID selects the optional Floating IP network.
	EnvExternalNetworkID = "GATEWAY_OPENSTACK_EXTERNAL_NETWORK_ID"
	// EnvMemberSubnetID selects the backend member subnet.
	EnvMemberSubnetID = "GATEWAY_OPENSTACK_MEMBER_SUBNET_ID"
	// EnvMemberMode selects the controller member mode.
	EnvMemberMode = "GATEWAY_OPENSTACK_MEMBER_MODE"
	// EnvNodeAddressType selects the Kubernetes Node address used for members.
	EnvNodeAddressType = "GATEWAY_OPENSTACK_NODE_ADDRESS_TYPE"
	// EnvHealthPath selects the backend health monitor request path.
	EnvHealthPath = "GATEWAY_OPENSTACK_HEALTH_PATH"
	// EnvResyncInterval selects the converged OpenStack observation interval.
	EnvResyncInterval = "GATEWAY_OPENSTACK_RESYNC_INTERVAL"
	// EnvAPIQPS selects the shared OpenStack client request rate.
	EnvAPIQPS = "GATEWAY_OPENSTACK_API_QPS"
	// EnvAPIBurst selects the shared OpenStack client request burst.
	EnvAPIBurst = "GATEWAY_OPENSTACK_API_BURST"
	// EnvCloudsYAML selects the mounted clouds.yaml file.
	EnvCloudsYAML = "OS_CLIENT_CONFIG_FILE"
	// EnvCloud selects the clouds.yaml entry.
	EnvCloud = "OS_CLOUD"
	// EnvRegion selects the OpenStack region.
	EnvRegion = "OS_REGION_NAME"
)

const (
	controllerProvider = "amphora"
	memberMode         = "NodePort"
	nodeAddressType    = "InternalIP"
	healthPath         = "/"
	resyncInterval     = "10m"
	apiQPS             = "10"
	apiBurst           = "20"
	cloudsYAMLPath     = CloudsMountPath + "/clouds.yaml"
)

var protectedArgumentNames = map[string]struct{}{
	"clouds-yaml":                 {},
	"cluster-id":                  {},
	"controller-name":             {},
	"external-network-id":         {},
	"health-path":                 {},
	"insecure":                    {},
	"member-mode":                 {},
	"member-subnet-id":            {},
	"node-address-type":           {},
	"octavia-provider":            {},
	"openstack-cloud":             {},
	"openstack-api-burst":         {},
	"openstack-api-qps":           {},
	"openstack-operation-timeout": {},
	"openstack-poll-interval":     {},
	"openstack-region":            {},
	"openstack-resync-interval":   {},
	"vip-subnet-id":               {},
}

var protectedEnvironmentNames = map[string]struct{}{
	EnvControllerName:    {},
	EnvClusterID:         {},
	EnvOctaviaProvider:   {},
	EnvVIPSubnetID:       {},
	EnvExternalNetworkID: {},
	EnvMemberSubnetID:    {},
	EnvMemberMode:        {},
	EnvNodeAddressType:   {},
	EnvHealthPath:        {},
	EnvResyncInterval:    {},
	EnvAPIQPS:            {},
	EnvAPIBurst:          {},
	EnvCloudsYAML:        {},
	EnvCloud:             {},
	EnvRegion:            {},
}

// Settings contains the values rendered into the controller ConfigMap for one
// E2E run.
type Settings struct {
	ControllerName    string
	ClusterID         string
	VIPSubnetID       string
	ExternalNetworkID string
	MemberSubnetID    string
	Cloud             string
	Region            string
}

// Environment returns a new map containing the complete protected controller
// environment for the settings.
func (s Settings) Environment() map[string]string {
	return map[string]string{
		EnvControllerName:    s.ControllerName,
		EnvClusterID:         s.ClusterID,
		EnvOctaviaProvider:   controllerProvider,
		EnvVIPSubnetID:       s.VIPSubnetID,
		EnvExternalNetworkID: s.ExternalNetworkID,
		EnvMemberSubnetID:    s.MemberSubnetID,
		EnvMemberMode:        memberMode,
		EnvNodeAddressType:   nodeAddressType,
		EnvHealthPath:        healthPath,
		EnvResyncInterval:    resyncInterval,
		EnvAPIQPS:            apiQPS,
		EnvAPIBurst:          apiBurst,
		EnvCloudsYAML:        cloudsYAMLPath,
		EnvCloud:             s.Cloud,
		EnvRegion:            s.Region,
	}
}

// IsProtectedEnvironment reports whether a direct environment variable could
// override a setting owned by the run-scoped ConfigMap.
func IsProtectedEnvironment(name string) bool {
	_, protected := protectedEnvironmentNames[name]
	return protected
}

// IsProtectedArgument reports whether a command line flag could override a
// setting owned by the run-scoped ConfigMap.
func IsProtectedArgument(name string) bool {
	_, protected := protectedArgumentNames[strings.TrimLeft(name, "-")]
	return protected
}

// RequiredArguments returns the exact controller arguments required by the
// E2E recovery and metrics checks.
func RequiredArguments(octaviaMicroversion string) ([]string, error) {
	if octaviaMicroversion == "" || strings.TrimSpace(octaviaMicroversion) != octaviaMicroversion {
		return nil, fmt.Errorf("octavia microversion must not be empty or contain surrounding whitespace")
	}
	return []string{
		"--leader-elect=true",
		"--metrics-bind-address=:8080",
		"--octavia-microversion=" + octaviaMicroversion,
	}, nil
}

// ValidateArguments rejects protected overrides and requires every argument
// needed by the E2E recovery and metrics checks exactly once.
func ValidateArguments(arguments []string, octaviaMicroversion string) error {
	required, err := RequiredArguments(octaviaMicroversion)
	if err != nil {
		return err
	}
	wanted := make(map[string]string, len(required))
	for _, argument := range required {
		wanted[argumentName(argument)] = argument
	}
	seen := make(map[string]int, len(wanted))
	for _, argument := range arguments {
		if !strings.HasPrefix(argument, "-") {
			continue
		}
		name := argumentName(argument)
		if expected, found := wanted[name]; found {
			seen[name]++
			if argument != expected {
				return fmt.Errorf("controller argument %s does not match the E2E contract", name)
			}
			continue
		}
		if IsProtectedArgument(name) {
			return fmt.Errorf("controller argument %s overrides a protected setting", name)
		}
	}
	for name := range wanted {
		if seen[name] != 1 {
			return fmt.Errorf("controller must set argument %s exactly once", name)
		}
	}
	return nil
}

// ValidateMetricsPorts requires one named TCP metrics port at the address used
// by the E2E Pod proxy checks.
func ValidateMetricsPorts(ports []corev1.ContainerPort) error {
	matches := 0
	for _, port := range ports {
		if port.Name != MetricsPortName {
			continue
		}
		matches++
		if port.ContainerPort != MetricsPort || port.Protocol != corev1.ProtocolTCP {
			return fmt.Errorf("controller has an unexpected metrics container port")
		}
	}
	if matches != 1 {
		return fmt.Errorf("controller must expose one named metrics container port")
	}
	return nil
}

// SourceVersion returns the annotation value that binds a Pod template to one
// immutable Kubernetes API object version.
func SourceVersion(uid types.UID, resourceVersion string) (string, error) {
	if uid == "" || strings.TrimSpace(string(uid)) != string(uid) || resourceVersion == "" ||
		strings.TrimSpace(resourceVersion) != resourceVersion {
		return "", fmt.Errorf("controller configuration source lacks API identity")
	}
	return string(uid) + "/" + resourceVersion, nil
}

func argumentName(argument string) string {
	return strings.TrimLeft(strings.SplitN(argument, "=", 2)[0], "-")
}
