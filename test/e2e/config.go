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

package e2e

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

const (
	enableEnvironment                    = "GATEWAY_OPENSTACK_E2E"
	projectModeEnvironment               = "GATEWAY_OPENSTACK_E2E_PROJECT_MODE"
	dedicatedProjectEnvironment          = "GATEWAY_OPENSTACK_E2E_DEDICATED_PROJECT"
	expectedProjectIDEnvironment         = "GATEWAY_OPENSTACK_E2E_EXPECTED_PROJECT_ID"
	expectedVIPSubnetIDEnvironment       = "GATEWAY_OPENSTACK_E2E_EXPECTED_VIP_SUBNET_ID"
	expectedMemberSubnetIDEnvironment    = "GATEWAY_OPENSTACK_E2E_EXPECTED_MEMBER_SUBNET_ID"
	expectedExternalNetworkIDEnvironment = "GATEWAY_OPENSTACK_E2E_EXPECTED_EXTERNAL_NETWORK_ID"
	controllerCloudsSecretEnvironment    = "GATEWAY_OPENSTACK_E2E_CONTROLLER_CLOUDS_SECRET"
	runIDEnvironment                     = "GATEWAY_OPENSTACK_E2E_RUN_ID"
	namespaceEnvironment                 = "GATEWAY_OPENSTACK_E2E_NAMESPACE"
	kubeconfigEnvironment                = "GATEWAY_OPENSTACK_E2E_KUBECONFIG"
	kubeContextEnvironment               = "GATEWAY_OPENSTACK_E2E_CONTEXT"
	controllerNameEnvironment            = "GATEWAY_OPENSTACK_E2E_CONTROLLER_NAME"
	controllerNamespaceEnvironment       = "GATEWAY_OPENSTACK_E2E_CONTROLLER_NAMESPACE"
	controllerDeploymentEnvironment      = "GATEWAY_OPENSTACK_E2E_CONTROLLER_DEPLOYMENT"
	controllerContainerEnvironment       = "GATEWAY_OPENSTACK_E2E_CONTROLLER_CONTAINER"
	controllerReplicasEnvironment        = "GATEWAY_OPENSTACK_E2E_CONTROLLER_REPLICAS"
	controllerImageDigestEnvironment     = "GATEWAY_OPENSTACK_E2E_CONTROLLER_IMAGE_DIGEST"
	controllerRevisionEnvironment        = "GATEWAY_OPENSTACK_E2E_CONTROLLER_REVISION"
	leaderLeaseEnvironment               = "GATEWAY_OPENSTACK_E2E_LEASE_NAME"
	backendImageEnvironment              = "GATEWAY_OPENSTACK_E2E_BACKEND_IMAGE"
	artifactDirectoryEnvironment         = "GATEWAY_OPENSTACK_E2E_ARTIFACT_DIR"
	restartModeEnvironment               = "GATEWAY_OPENSTACK_E2E_RESTART_MODE"
	timeoutEnvironment                   = "GATEWAY_OPENSTACK_E2E_TIMEOUT"
	pollIntervalEnvironment              = "GATEWAY_OPENSTACK_E2E_POLL_INTERVAL"
	httpTimeoutEnvironment               = "GATEWAY_OPENSTACK_E2E_HTTP_TIMEOUT"
	noOpWindowEnvironment                = "GATEWAY_OPENSTACK_E2E_NOOP_WINDOW"
	auditEnableEnvironment               = "GATEWAY_OPENSTACK_E2E_AUDIT"
	auditBinaryEnvironment               = "GATEWAY_OPENSTACK_E2E_AUDIT_BINARY"
	clusterIDEnvironment                 = "GATEWAY_OPENSTACK_E2E_CLUSTER_ID"
	auditCloudsYAMLEnvironment           = "GATEWAY_OPENSTACK_E2E_AUDIT_CLOUDS_YAML"
	auditCloudEnvironment                = "GATEWAY_OPENSTACK_E2E_AUDIT_CLOUD"
	auditRegionEnvironment               = "GATEWAY_OPENSTACK_E2E_AUDIT_REGION"
	auditMicroversionEnvironment         = "GATEWAY_OPENSTACK_E2E_AUDIT_OCTAVIA_MICROVERSION"
	dedicatedRunAnnotation               = "e2e.gateway-api-openstack.io/run-id"
	testNamespacePrefix                  = "gateway-api-openstack-e2e-"
	defaultRestartMode                   = "cold"
	defaultTimeout                       = 45 * time.Minute
	maximumTimeout                       = 50 * time.Minute
	defaultPollInterval                  = 5 * time.Second
	defaultHTTPTimeout                   = 10 * time.Second
	defaultNoOpWindow                    = 30 * time.Second
	defaultAuditMicroversion             = "2.5"
	sharedExternalNetworkNone            = "none"
	testIdentityPrefix                   = "gao-e2e-"
	minimumRunIDLength                   = 8
	maximumRunIDLength                   = 32
)

var (
	sha256DigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	digestImagePattern  = regexp.MustCompile(`^[^\s@]+@sha256:[a-f0-9]{64}$`)
	agnhostImagePattern = regexp.MustCompile(`^(?:[^\s/@]+/)*agnhost(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?@sha256:[a-f0-9]{64}$`)
	gitRevisionPattern  = regexp.MustCompile(`^[a-f0-9]{40}$`)
)

type environmentReader func(string) string

type projectMode string

const (
	projectModeDedicated projectMode = "dedicated"
	projectModeShared    projectMode = "shared"
)

type projectConfig struct {
	mode                      projectMode
	expectedProjectID         string
	expectedVIPSubnetID       string
	expectedMemberSubnetID    string
	expectedExternalNetworkID string
	externalNetworkSet        bool
	controllerCloudsSecret    string
}

type e2eConfig struct {
	runID                 string
	namespace             string
	kubeconfig            string
	kubeContext           string
	controllerName        string
	controllerNamespace   string
	controllerDeployment  string
	controllerContainer   string
	controllerReplicas    int32
	controllerImageDigest string
	controllerRevision    string
	leaderLease           string
	backendImage          string
	artifactDirectory     string
	restartMode           string
	timeout               time.Duration
	pollInterval          time.Duration
	httpTimeout           time.Duration
	noOpWindow            time.Duration
	project               projectConfig
	audit                 auditConfig
}

type auditConfig struct {
	enabled      bool
	binary       string
	clusterID    string
	cloudsYAML   string
	cloud        string
	region       string
	microversion string
}

func loadE2EConfig(getenv environmentReader) (e2eConfig, bool, error) {
	if getenv(enableEnvironment) != "true" {
		return e2eConfig{}, false, nil
	}

	config := e2eConfig{
		runID:                 strings.TrimSpace(getenv(runIDEnvironment)),
		namespace:             strings.TrimSpace(getenv(namespaceEnvironment)),
		kubeconfig:            strings.TrimSpace(getenv(kubeconfigEnvironment)),
		kubeContext:           strings.TrimSpace(getenv(kubeContextEnvironment)),
		controllerName:        strings.TrimSpace(getenv(controllerNameEnvironment)),
		controllerNamespace:   strings.TrimSpace(getenv(controllerNamespaceEnvironment)),
		controllerDeployment:  strings.TrimSpace(getenv(controllerDeploymentEnvironment)),
		controllerContainer:   strings.TrimSpace(getenv(controllerContainerEnvironment)),
		controllerImageDigest: strings.TrimSpace(getenv(controllerImageDigestEnvironment)),
		controllerRevision:    strings.TrimSpace(getenv(controllerRevisionEnvironment)),
		leaderLease:           strings.TrimSpace(getenv(leaderLeaseEnvironment)),
		backendImage:          strings.TrimSpace(getenv(backendImageEnvironment)),
		artifactDirectory:     strings.TrimSpace(getenv(artifactDirectoryEnvironment)),
		restartMode:           valueOrDefault(getenv(restartModeEnvironment), defaultRestartMode),
	}
	var err error
	if config.controllerReplicas, err = parseInt32(getenv(controllerReplicasEnvironment), controllerReplicasEnvironment); err != nil {
		return e2eConfig{}, true, err
	}
	if config.timeout, err = parseDurationOrDefault(getenv(timeoutEnvironment), timeoutEnvironment, defaultTimeout); err != nil {
		return e2eConfig{}, true, err
	}
	if config.pollInterval, err = parseDurationOrDefault(getenv(pollIntervalEnvironment), pollIntervalEnvironment, defaultPollInterval); err != nil {
		return e2eConfig{}, true, err
	}
	if config.httpTimeout, err = parseDurationOrDefault(getenv(httpTimeoutEnvironment), httpTimeoutEnvironment, defaultHTTPTimeout); err != nil {
		return e2eConfig{}, true, err
	}
	if config.noOpWindow, err = parseDurationOrDefault(getenv(noOpWindowEnvironment), noOpWindowEnvironment, defaultNoOpWindow); err != nil {
		return e2eConfig{}, true, err
	}
	if config.project, err = loadProjectConfig(getenv); err != nil {
		return e2eConfig{}, true, err
	}
	if config.audit, err = loadAuditConfig(getenv); err != nil {
		return e2eConfig{}, true, err
	}
	if err := config.validate(); err != nil {
		return e2eConfig{}, true, err
	}
	return config, true, nil
}

func loadProjectConfig(getenv environmentReader) (projectConfig, error) {
	modeValue := getenv(projectModeEnvironment)
	legacyValue := getenv(dedicatedProjectEnvironment)
	if modeValue == "" {
		if err := requireExactTrue(legacyValue, dedicatedProjectEnvironment); err != nil {
			return projectConfig{}, fmt.Errorf("%s must be set to dedicated or shared, or the legacy dedicated-project acknowledgement must be exactly true", projectModeEnvironment)
		}
		return projectConfig{mode: projectModeDedicated}, nil
	}

	config := projectConfig{mode: projectMode(modeValue)}
	switch config.mode {
	case projectModeDedicated:
		if err := requireExactTrue(legacyValue, dedicatedProjectEnvironment); err != nil {
			return projectConfig{}, err
		}
	case projectModeShared:
		if legacyValue != "" && legacyValue != "false" {
			return projectConfig{}, fmt.Errorf("%s must be unset or exactly false in shared-project mode", dedicatedProjectEnvironment)
		}
		config.expectedProjectID = strings.TrimSpace(getenv(expectedProjectIDEnvironment))
		config.expectedVIPSubnetID = strings.TrimSpace(getenv(expectedVIPSubnetIDEnvironment))
		config.expectedMemberSubnetID = strings.TrimSpace(getenv(expectedMemberSubnetIDEnvironment))
		externalNetwork := strings.TrimSpace(getenv(expectedExternalNetworkIDEnvironment))
		config.externalNetworkSet = externalNetwork != ""
		if externalNetwork == sharedExternalNetworkNone {
			config.expectedExternalNetworkID = ""
		} else {
			config.expectedExternalNetworkID = externalNetwork
		}
		config.controllerCloudsSecret = strings.TrimSpace(getenv(controllerCloudsSecretEnvironment))
	default:
		return projectConfig{}, fmt.Errorf("%s must be exactly dedicated or shared", projectModeEnvironment)
	}
	return config, nil
}

func loadAuditConfig(getenv environmentReader) (auditConfig, error) {
	enabled, err := parseOptionalBool(getenv(auditEnableEnvironment), auditEnableEnvironment)
	if err != nil {
		return auditConfig{}, err
	}
	config := auditConfig{enabled: enabled}
	if !enabled {
		return config, nil
	}
	config.binary = strings.TrimSpace(getenv(auditBinaryEnvironment))
	config.clusterID = strings.TrimSpace(getenv(clusterIDEnvironment))
	config.cloudsYAML = strings.TrimSpace(getenv(auditCloudsYAMLEnvironment))
	config.cloud = strings.TrimSpace(getenv(auditCloudEnvironment))
	config.region = strings.TrimSpace(getenv(auditRegionEnvironment))
	config.microversion = valueOrDefault(getenv(auditMicroversionEnvironment), defaultAuditMicroversion)
	return config, nil
}

func (c e2eConfig) validate() error {
	if len(c.runID) < minimumRunIDLength || len(c.runID) > maximumRunIDLength || len(validation.IsDNS1123Label(c.runID)) != 0 {
		return fmt.Errorf("%s must be a DNS label between %d and %d characters", runIDEnvironment, minimumRunIDLength, maximumRunIDLength)
	}
	if expected := testNamespacePrefix + c.runID; c.namespace != expected {
		return fmt.Errorf("%s must be exactly %q", namespaceEnvironment, expected)
	}
	if c.kubeconfig == "" || !filepath.IsAbs(c.kubeconfig) {
		return fmt.Errorf("%s must be an absolute path", kubeconfigEnvironment)
	}
	if c.kubeContext == "" {
		return fmt.Errorf("%s must not be empty", kubeContextEnvironment)
	}
	if len(validation.IsDomainPrefixedPath(field.NewPath("controller-name"), c.controllerName)) != 0 {
		return fmt.Errorf("%s must be a domain-prefixed path", controllerNameEnvironment)
	}
	if len(validation.IsDNS1123Label(c.controllerNamespace)) != 0 {
		return fmt.Errorf("%s must be a DNS label", controllerNamespaceEnvironment)
	}
	if c.controllerNamespace == c.namespace {
		return fmt.Errorf("the controller and test workload must use different namespaces")
	}
	for _, item := range []struct {
		name  string
		value string
	}{
		{name: controllerDeploymentEnvironment, value: c.controllerDeployment},
		{name: leaderLeaseEnvironment, value: c.leaderLease},
	} {
		if len(validation.IsDNS1123Subdomain(item.value)) != 0 {
			return fmt.Errorf("%s must be a DNS subdomain", item.name)
		}
	}
	if len(validation.IsDNS1123Label(c.controllerContainer)) != 0 {
		return fmt.Errorf("%s must be a DNS label", controllerContainerEnvironment)
	}
	if c.controllerReplicas < 2 {
		return fmt.Errorf("%s must be at least 2 to prove leader failover", controllerReplicasEnvironment)
	}
	if !sha256DigestPattern.MatchString(c.controllerImageDigest) {
		return fmt.Errorf("%s must be a lowercase sha256 digest", controllerImageDigestEnvironment)
	}
	if !gitRevisionPattern.MatchString(c.controllerRevision) {
		return fmt.Errorf("%s must be a full 40-character lowercase Git commit", controllerRevisionEnvironment)
	}
	if !digestImagePattern.MatchString(c.backendImage) {
		return fmt.Errorf("%s must pin the backend image by sha256 digest", backendImageEnvironment)
	}
	if !agnhostImagePattern.MatchString(c.backendImage) {
		return fmt.Errorf("%s must use a digest-pinned agnhost image", backendImageEnvironment)
	}
	if c.artifactDirectory == "" || !filepath.IsAbs(c.artifactDirectory) || filepath.Clean(c.artifactDirectory) == string(filepath.Separator) {
		return fmt.Errorf("%s must be a non-root absolute path", artifactDirectoryEnvironment)
	}
	if filepath.Base(filepath.Clean(c.artifactDirectory)) != c.runID {
		return fmt.Errorf("%s must end with the exact E2E run ID", artifactDirectoryEnvironment)
	}
	if c.restartMode != defaultRestartMode {
		return fmt.Errorf("%s must be %q", restartModeEnvironment, defaultRestartMode)
	}
	if c.pollInterval <= 0 || c.httpTimeout <= 0 || c.noOpWindow <= 0 || c.timeout <= c.pollInterval {
		return fmt.Errorf("E2E timeout and polling durations must be positive and the timeout must exceed the poll interval")
	}
	if c.timeout > maximumTimeout {
		return fmt.Errorf("%s must not exceed %s so emergency cleanup can finish before the outer test timeout", timeoutEnvironment, maximumTimeout)
	}
	if err := c.audit.validate(); err != nil {
		return err
	}
	if err := c.project.validate(c); err != nil {
		return err
	}
	return nil
}

func (c projectConfig) validate(config e2eConfig) error {
	if c.mode == projectModeDedicated {
		return nil
	}
	if c.mode != projectModeShared {
		return fmt.Errorf("project mode is invalid")
	}
	for _, item := range []struct {
		name  string
		value string
	}{
		{name: expectedProjectIDEnvironment, value: c.expectedProjectID},
		{name: expectedVIPSubnetIDEnvironment, value: c.expectedVIPSubnetID},
		{name: expectedMemberSubnetIDEnvironment, value: c.expectedMemberSubnetID},
		{name: controllerCloudsSecretEnvironment, value: c.controllerCloudsSecret},
	} {
		if item.value == "" {
			return fmt.Errorf("%s must not be empty in shared-project mode", item.name)
		}
	}
	if !c.externalNetworkSet {
		return fmt.Errorf("%s must be an OpenStack network ID or exactly %q in shared-project mode", expectedExternalNetworkIDEnvironment, sharedExternalNetworkNone)
	}
	if len(validation.IsDNS1123Subdomain(c.controllerCloudsSecret)) != 0 {
		return fmt.Errorf("%s must be a DNS subdomain", controllerCloudsSecretEnvironment)
	}
	if !config.audit.enabled {
		return fmt.Errorf("%s must be exactly true in shared-project mode", auditEnableEnvironment)
	}
	if config.audit.cloudsYAML == "" || config.audit.cloud == "" || config.audit.region == "" {
		return fmt.Errorf("shared-project mode requires explicit audit clouds.yaml, cloud, and region settings")
	}
	expectedIdentity := testIdentityPrefix + config.runID
	_, controllerPath, found := strings.Cut(config.controllerName, "/")
	if !found || controllerPath != expectedIdentity {
		return fmt.Errorf("%s must end with the exact run identity in shared-project mode", controllerNameEnvironment)
	}
	if config.audit.clusterID != expectedIdentity {
		return fmt.Errorf("%s must be the exact run identity in shared-project mode", clusterIDEnvironment)
	}
	if config.controllerNamespace != "openstack-gateway-e2e-"+config.runID {
		return fmt.Errorf("%s must be derived from the run ID in shared-project mode", controllerNamespaceEnvironment)
	}
	return nil
}

func (c auditConfig) validate() error {
	if !c.enabled {
		return nil
	}
	if c.binary == "" || !filepath.IsAbs(c.binary) {
		return fmt.Errorf("%s must be an absolute path when audit is enabled", auditBinaryEnvironment)
	}
	if c.clusterID == "" {
		return fmt.Errorf("%s must not be empty when audit is enabled", clusterIDEnvironment)
	}
	if c.cloudsYAML != "" && !filepath.IsAbs(c.cloudsYAML) {
		return fmt.Errorf("%s must be an absolute path", auditCloudsYAMLEnvironment)
	}
	if c.cloudsYAML != "" && c.cloud == "" {
		return fmt.Errorf("%s is required with %s", auditCloudEnvironment, auditCloudsYAMLEnvironment)
	}
	if c.microversion == "" {
		return fmt.Errorf("%s must not be empty", auditMicroversionEnvironment)
	}
	return nil
}

func parseOptionalBool(value, name string) (bool, error) {
	value = strings.TrimSpace(value)
	switch value {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, fmt.Errorf("%s must be true or false", name)
	}
}

func requireExactTrue(value, name string) error {
	if value != "true" {
		return fmt.Errorf("%s must be exactly true", name)
	}
	return nil
}

func parseInt32(value, name string) (int32, error) {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return int32(parsed), nil
}

func parseDurationOrDefault(value, name string, fallback time.Duration) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration", name)
	}
	return parsed, nil
}

func valueOrDefault(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}
