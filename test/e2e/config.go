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
	enableEnvironment                = "GATEWAY_OPENSTACK_E2E"
	dedicatedProjectEnvironment      = "GATEWAY_OPENSTACK_E2E_DEDICATED_PROJECT"
	runIDEnvironment                 = "GATEWAY_OPENSTACK_E2E_RUN_ID"
	namespaceEnvironment             = "GATEWAY_OPENSTACK_E2E_NAMESPACE"
	kubeconfigEnvironment            = "GATEWAY_OPENSTACK_E2E_KUBECONFIG"
	kubeContextEnvironment           = "GATEWAY_OPENSTACK_E2E_CONTEXT"
	controllerNameEnvironment        = "GATEWAY_OPENSTACK_E2E_CONTROLLER_NAME"
	controllerNamespaceEnvironment   = "GATEWAY_OPENSTACK_E2E_CONTROLLER_NAMESPACE"
	controllerDeploymentEnvironment  = "GATEWAY_OPENSTACK_E2E_CONTROLLER_DEPLOYMENT"
	controllerContainerEnvironment   = "GATEWAY_OPENSTACK_E2E_CONTROLLER_CONTAINER"
	controllerReplicasEnvironment    = "GATEWAY_OPENSTACK_E2E_CONTROLLER_REPLICAS"
	controllerImageDigestEnvironment = "GATEWAY_OPENSTACK_E2E_CONTROLLER_IMAGE_DIGEST"
	controllerRevisionEnvironment    = "GATEWAY_OPENSTACK_E2E_CONTROLLER_REVISION"
	leaderLeaseEnvironment           = "GATEWAY_OPENSTACK_E2E_LEASE_NAME"
	backendImageEnvironment          = "GATEWAY_OPENSTACK_E2E_BACKEND_IMAGE"
	artifactDirectoryEnvironment     = "GATEWAY_OPENSTACK_E2E_ARTIFACT_DIR"
	restartModeEnvironment           = "GATEWAY_OPENSTACK_E2E_RESTART_MODE"
	timeoutEnvironment               = "GATEWAY_OPENSTACK_E2E_TIMEOUT"
	pollIntervalEnvironment          = "GATEWAY_OPENSTACK_E2E_POLL_INTERVAL"
	httpTimeoutEnvironment           = "GATEWAY_OPENSTACK_E2E_HTTP_TIMEOUT"
	noOpWindowEnvironment            = "GATEWAY_OPENSTACK_E2E_NOOP_WINDOW"
	auditEnableEnvironment           = "GATEWAY_OPENSTACK_E2E_AUDIT"
	auditBinaryEnvironment           = "GATEWAY_OPENSTACK_E2E_AUDIT_BINARY"
	clusterIDEnvironment             = "GATEWAY_OPENSTACK_E2E_CLUSTER_ID"
	auditCloudsYAMLEnvironment       = "GATEWAY_OPENSTACK_E2E_AUDIT_CLOUDS_YAML"
	auditCloudEnvironment            = "GATEWAY_OPENSTACK_E2E_AUDIT_CLOUD"
	auditRegionEnvironment           = "GATEWAY_OPENSTACK_E2E_AUDIT_REGION"
	auditMicroversionEnvironment     = "GATEWAY_OPENSTACK_E2E_AUDIT_OCTAVIA_MICROVERSION"
	dedicatedRunAnnotation           = "e2e.gateway-api-openstack.io/run-id"
	testNamespacePrefix              = "gateway-api-openstack-e2e-"
	defaultRestartMode               = "cold"
	defaultTimeout                   = 45 * time.Minute
	maximumTimeout                   = 50 * time.Minute
	defaultPollInterval              = 5 * time.Second
	defaultHTTPTimeout               = 10 * time.Second
	defaultNoOpWindow                = 30 * time.Second
	defaultAuditMicroversion         = "2.5"
	minimumRunIDLength               = 8
	maximumRunIDLength               = 32
)

var (
	sha256DigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	digestImagePattern  = regexp.MustCompile(`^[^\s@]+@sha256:[a-f0-9]{64}$`)
	agnhostImagePattern = regexp.MustCompile(`^(?:[^\s/@]+/)*agnhost(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?@sha256:[a-f0-9]{64}$`)
	gitRevisionPattern  = regexp.MustCompile(`^[a-f0-9]{40}$`)
)

type environmentReader func(string) string

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
	if config.audit, err = loadAuditConfig(getenv); err != nil {
		return e2eConfig{}, true, err
	}
	if err := requireExactTrue(getenv(dedicatedProjectEnvironment), dedicatedProjectEnvironment); err != nil {
		return e2eConfig{}, true, err
	}
	if err := config.validate(); err != nil {
		return e2eConfig{}, true, err
	}
	return config, true, nil
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
