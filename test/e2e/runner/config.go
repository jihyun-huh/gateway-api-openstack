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

package main

import (
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

const (
	configFormatVersion = "v1alpha1"
	sharedProjectMode   = "shared"
	disabledNetwork     = "none"

	runAnnotation = "e2e.gateway-api-openstack.io/run-id"

	workloadNamespacePrefix   = "gateway-api-openstack-e2e-"
	controllerNamespacePrefix = "openstack-gateway-e2e-"
	runIdentityPrefix         = "gao-e2e-"

	controllerDeploymentName = "openstack-gateway-controller"
	controllerContainerName  = "controller"
	controllerConfigMapName  = "openstack-gateway-controller-config"
	controllerSecretName     = "openstack-clouds"
	controllerServiceAccount = "openstack-gateway-controller"
	controllerLeaseName      = "gateway-api-openstack-controller"
	controllerReplicas       = int32(2)
	octaviaMicroversion      = "2.5"
)

var (
	digestImagePattern  = regexp.MustCompile(`^[^\s@]+@(sha256:[a-f0-9]{64})$`)
	agnhostImagePattern = regexp.MustCompile(`^(?:[^\s/@]+/)*agnhost(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?@sha256:[a-f0-9]{64}$`)
	gitRevisionPattern  = regexp.MustCompile(`^[a-f0-9]{40}$`)
)

type fileConfig struct {
	FormatVersion string           `json:"formatVersion"`
	RunID         string           `json:"runID,omitempty"`
	Kubernetes    kubernetesConfig `json:"kubernetes"`
	OpenStack     openStackConfig  `json:"openstack"`
	Controller    controllerConfig `json:"controller"`
	Backend       backendConfig    `json:"backend"`
	Artifacts     artifactsConfig  `json:"artifacts,omitempty"`
}

type kubernetesConfig struct {
	DedicatedForE2E bool   `json:"dedicatedForE2E"`
	Kubeconfig      string `json:"kubeconfig"`
	Context         string `json:"context"`
}

type openStackConfig struct {
	ProjectMode                     string `json:"projectMode"`
	AcceptProjectWideCredentialRisk bool   `json:"acceptProjectWideCredentialRisk"`
	ExpectedProjectID               string `json:"expectedProjectID"`
	Cloud                           string `json:"cloud"`
	Region                          string `json:"region"`
	ControllerCloudsYAML            string `json:"controllerCloudsYAML"`
	AuditCloudsYAML                 string `json:"auditCloudsYAML,omitempty"`
	VIPSubnetID                     string `json:"vipSubnetID"`
	MemberSubnetID                  string `json:"memberSubnetID"`
	ExternalNetworkID               string `json:"externalNetworkID"`
}

type controllerConfig struct {
	Domain         string `json:"domain"`
	Image          string `json:"image"`
	SourceRevision string `json:"sourceRevision"`
}

type backendConfig struct {
	Image string `json:"image"`
}

type artifactsConfig struct {
	Root string `json:"root,omitempty"`
}

type resolvedConfig struct {
	runID                  string
	workloadNamespace      string
	controllerNamespace    string
	gatewayClassName       string
	controllerName         string
	clusterID              string
	clusterRoleName        string
	clusterRoleBindingName string

	kubeconfig  string
	kubeContext string

	expectedProjectID    string
	cloud                string
	region               string
	controllerCloudsYAML string
	auditCloudsYAML      string
	vipSubnetID          string
	memberSubnetID       string
	externalNetworkInput string
	externalNetworkID    string

	controllerImage          string
	controllerImageDigest    string
	controllerSourceRevision string
	backendImage             string
	artifactDirectory        string
	auditBinary              string
}

type resolveOptions struct {
	repositoryRoot string
	now            func() time.Time
	random         io.Reader
}

func loadFileConfig(path string) (fileConfig, error) {
	if path == "" || !filepath.IsAbs(path) {
		return fileConfig{}, fmt.Errorf("e2e config path must be absolute")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return fileConfig{}, fmt.Errorf("read e2e config: %w", err)
	}
	var config fileConfig
	if err := yaml.UnmarshalStrict(contents, &config); err != nil {
		return fileConfig{}, fmt.Errorf("decode e2e config: %w", err)
	}
	return config, nil
}

func resolveFileConfig(config fileConfig, options resolveOptions) (resolvedConfig, error) {
	if options.repositoryRoot == "" || !filepath.IsAbs(options.repositoryRoot) {
		return resolvedConfig{}, fmt.Errorf("repository root must be absolute")
	}
	if options.now == nil {
		options.now = time.Now
	}
	if options.random == nil {
		options.random = rand.Reader
	}

	if config.FormatVersion != configFormatVersion {
		return resolvedConfig{}, fmt.Errorf("formatVersion must be exactly %q", configFormatVersion)
	}
	if !config.Kubernetes.DedicatedForE2E {
		return resolvedConfig{}, fmt.Errorf("kubernetes.dedicatedForE2E must be exactly true")
	}
	if config.OpenStack.ProjectMode != sharedProjectMode {
		return resolvedConfig{}, fmt.Errorf("openstack.projectMode must be exactly %q", sharedProjectMode)
	}
	if !config.OpenStack.AcceptProjectWideCredentialRisk {
		return resolvedConfig{}, fmt.Errorf("openstack.acceptProjectWideCredentialRisk must be exactly true")
	}

	runID := strings.TrimSpace(config.RunID)
	if runID == "" {
		generated, err := generateRunID(options.now(), options.random)
		if err != nil {
			return resolvedConfig{}, err
		}
		runID = generated
	}
	if len(runID) < 8 || len(runID) > 32 || len(validation.IsDNS1123Label(runID)) != 0 {
		return resolvedConfig{}, fmt.Errorf("runID must be a DNS label between 8 and 32 characters")
	}

	kubeconfig := strings.TrimSpace(config.Kubernetes.Kubeconfig)
	if kubeconfig == "" || !filepath.IsAbs(kubeconfig) {
		return resolvedConfig{}, fmt.Errorf("kubernetes.kubeconfig must be an absolute path")
	}
	kubeContext := strings.TrimSpace(config.Kubernetes.Context)
	if kubeContext == "" {
		return resolvedConfig{}, fmt.Errorf("kubernetes.context must not be empty")
	}

	domain := strings.TrimSpace(config.Controller.Domain)
	if len(validation.IsDNS1123Subdomain(domain)) != 0 {
		return resolvedConfig{}, fmt.Errorf("controller.domain must be a DNS subdomain")
	}
	image := strings.TrimSpace(config.Controller.Image)
	imageMatch := digestImagePattern.FindStringSubmatch(image)
	if len(imageMatch) != 2 {
		return resolvedConfig{}, fmt.Errorf("controller.image must be pinned by a lowercase sha256 digest")
	}
	revision := strings.TrimSpace(config.Controller.SourceRevision)
	if !gitRevisionPattern.MatchString(revision) {
		return resolvedConfig{}, fmt.Errorf("controller.sourceRevision must be a full 40-character lowercase Git commit")
	}
	backendImage := strings.TrimSpace(config.Backend.Image)
	if !agnhostImagePattern.MatchString(backendImage) {
		return resolvedConfig{}, fmt.Errorf("backend.image must be a digest-pinned agnhost image")
	}

	for _, item := range []struct {
		name  string
		value string
	}{
		{name: "openstack.expectedProjectID", value: config.OpenStack.ExpectedProjectID},
		{name: "openstack.cloud", value: config.OpenStack.Cloud},
		{name: "openstack.region", value: config.OpenStack.Region},
		{name: "openstack.vipSubnetID", value: config.OpenStack.VIPSubnetID},
		{name: "openstack.memberSubnetID", value: config.OpenStack.MemberSubnetID},
		{name: "openstack.externalNetworkID", value: config.OpenStack.ExternalNetworkID},
	} {
		if strings.TrimSpace(item.value) == "" {
			return resolvedConfig{}, fmt.Errorf("%s must not be empty", item.name)
		}
	}
	controllerCloudsYAML := strings.TrimSpace(config.OpenStack.ControllerCloudsYAML)
	if controllerCloudsYAML == "" || !filepath.IsAbs(controllerCloudsYAML) {
		return resolvedConfig{}, fmt.Errorf("openstack.controllerCloudsYAML must be an absolute path")
	}
	auditCloudsYAML := strings.TrimSpace(config.OpenStack.AuditCloudsYAML)
	if auditCloudsYAML == "" {
		auditCloudsYAML = controllerCloudsYAML
	}
	if !filepath.IsAbs(auditCloudsYAML) {
		return resolvedConfig{}, fmt.Errorf("openstack.auditCloudsYAML must be an absolute path")
	}

	externalNetworkInput := strings.TrimSpace(config.OpenStack.ExternalNetworkID)
	externalNetworkID := externalNetworkInput
	if externalNetworkInput == disabledNetwork {
		externalNetworkID = ""
	}

	artifactsRoot := strings.TrimSpace(config.Artifacts.Root)
	if artifactsRoot == "" {
		artifactsRoot = filepath.Join(options.repositoryRoot, "_artifacts", "e2e")
	}
	if !filepath.IsAbs(artifactsRoot) || filepath.Clean(artifactsRoot) == string(filepath.Separator) {
		return resolvedConfig{}, fmt.Errorf("artifacts.root must be a non-root absolute path")
	}

	identity := runIdentityPrefix + runID
	controllerName := domain + "/" + identity
	if len(controllerName) > 253 {
		return resolvedConfig{}, fmt.Errorf("derived controller name must not exceed 253 characters")
	}
	return resolvedConfig{
		runID:                    runID,
		workloadNamespace:        workloadNamespacePrefix + runID,
		controllerNamespace:      controllerNamespacePrefix + runID,
		gatewayClassName:         identity,
		controllerName:           controllerName,
		clusterID:                identity,
		clusterRoleName:          identity + "-controller",
		clusterRoleBindingName:   identity + "-controller",
		kubeconfig:               kubeconfig,
		kubeContext:              kubeContext,
		expectedProjectID:        strings.TrimSpace(config.OpenStack.ExpectedProjectID),
		cloud:                    strings.TrimSpace(config.OpenStack.Cloud),
		region:                   strings.TrimSpace(config.OpenStack.Region),
		controllerCloudsYAML:     controllerCloudsYAML,
		auditCloudsYAML:          auditCloudsYAML,
		vipSubnetID:              strings.TrimSpace(config.OpenStack.VIPSubnetID),
		memberSubnetID:           strings.TrimSpace(config.OpenStack.MemberSubnetID),
		externalNetworkInput:     externalNetworkInput,
		externalNetworkID:        externalNetworkID,
		controllerImage:          image,
		controllerImageDigest:    imageMatch[1],
		controllerSourceRevision: revision,
		backendImage:             backendImage,
		artifactDirectory:        filepath.Join(filepath.Clean(artifactsRoot), runID),
		auditBinary:              filepath.Join(options.repositoryRoot, "bin", "openstack-gateway-audit"),
	}, nil
}

func generateRunID(now time.Time, random io.Reader) (string, error) {
	suffix := make([]byte, 4)
	if _, err := io.ReadFull(random, suffix); err != nil {
		return "", fmt.Errorf("generate e2e run ID: %w", err)
	}
	return fmt.Sprintf("run-%s-%x", now.UTC().Format("20060102-150405"), suffix), nil
}

func (c resolvedConfig) harnessEnvironment() map[string]string {
	return map[string]string{
		"GATEWAY_OPENSTACK_E2E":                              "true",
		"GATEWAY_OPENSTACK_E2E_PROJECT_MODE":                 sharedProjectMode,
		"GATEWAY_OPENSTACK_E2E_DEDICATED_PROJECT":            "false",
		"GATEWAY_OPENSTACK_E2E_EXPECTED_PROJECT_ID":          c.expectedProjectID,
		"GATEWAY_OPENSTACK_E2E_EXPECTED_VIP_SUBNET_ID":       c.vipSubnetID,
		"GATEWAY_OPENSTACK_E2E_EXPECTED_MEMBER_SUBNET_ID":    c.memberSubnetID,
		"GATEWAY_OPENSTACK_E2E_EXPECTED_EXTERNAL_NETWORK_ID": c.externalNetworkInput,
		"GATEWAY_OPENSTACK_E2E_CONTROLLER_CLOUDS_SECRET":     controllerSecretName,
		"GATEWAY_OPENSTACK_E2E_RUN_ID":                       c.runID,
		"GATEWAY_OPENSTACK_E2E_NAMESPACE":                    c.workloadNamespace,
		"GATEWAY_OPENSTACK_E2E_KUBECONFIG":                   c.kubeconfig,
		"GATEWAY_OPENSTACK_E2E_CONTEXT":                      c.kubeContext,
		"GATEWAY_OPENSTACK_E2E_CONTROLLER_NAME":              c.controllerName,
		"GATEWAY_OPENSTACK_E2E_CONTROLLER_NAMESPACE":         c.controllerNamespace,
		"GATEWAY_OPENSTACK_E2E_CONTROLLER_DEPLOYMENT":        controllerDeploymentName,
		"GATEWAY_OPENSTACK_E2E_CONTROLLER_CONTAINER":         controllerContainerName,
		"GATEWAY_OPENSTACK_E2E_CONTROLLER_REPLICAS":          fmt.Sprintf("%d", controllerReplicas),
		"GATEWAY_OPENSTACK_E2E_CONTROLLER_IMAGE_DIGEST":      c.controllerImageDigest,
		"GATEWAY_OPENSTACK_E2E_CONTROLLER_REVISION":          c.controllerSourceRevision,
		"GATEWAY_OPENSTACK_E2E_LEASE_NAME":                   controllerLeaseName,
		"GATEWAY_OPENSTACK_E2E_BACKEND_IMAGE":                c.backendImage,
		"GATEWAY_OPENSTACK_E2E_ARTIFACT_DIR":                 c.artifactDirectory,
		"GATEWAY_OPENSTACK_E2E_AUDIT":                        "true",
		"GATEWAY_OPENSTACK_E2E_AUDIT_BINARY":                 c.auditBinary,
		"GATEWAY_OPENSTACK_E2E_CLUSTER_ID":                   c.clusterID,
		"GATEWAY_OPENSTACK_E2E_AUDIT_CLOUDS_YAML":            c.auditCloudsYAML,
		"GATEWAY_OPENSTACK_E2E_AUDIT_CLOUD":                  c.cloud,
		"GATEWAY_OPENSTACK_E2E_AUDIT_REGION":                 c.region,
		"GATEWAY_OPENSTACK_E2E_AUDIT_OCTAVIA_MICROVERSION":   octaviaMicroversion,
	}
}
