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

// Package runconfig loads and resolves the private configuration for a live
// OpenStack E2E run.
package runconfig

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
	// FormatVersion is the accepted user and runtime configuration version.
	FormatVersion = "v1alpha1"
	// EnableEnvironment is the exact opt-in consumed by the live test process.
	EnableEnvironment = "GATEWAY_OPENSTACK_E2E"
	// RuntimeConfigEnvironment points the live test process at its private runtime document.
	RuntimeConfigEnvironment = "GATEWAY_OPENSTACK_E2E_RUNTIME_CONFIG"

	// ProjectModeDedicated selects an OpenStack project reserved for E2E.
	ProjectModeDedicated ProjectMode = "dedicated"
	// ProjectModeShared selects an OpenStack project used by other workloads.
	ProjectModeShared ProjectMode = "shared"

	disabledNetwork = "none"

	workloadNamespacePrefix   = "gateway-api-openstack-e2e-"
	controllerNamespacePrefix = "openstack-gateway-e2e-"
	runIdentityPrefix         = "gao-e2e-"

	controllerDeploymentName = "openstack-gateway-controller"
	controllerContainerName  = "controller"
	controllerSecretName     = "openstack-clouds"
	controllerLeaseName      = "gateway-api-openstack-controller"
	controllerReplicas       = int32(2)
	octaviaMicroversion      = "2.5"

	defaultRestartMode  = "cold"
	defaultTimeout      = 45 * time.Minute
	defaultPollInterval = 5 * time.Second
	defaultHTTPTimeout  = 10 * time.Second
	defaultNoOpWindow   = 30 * time.Second
)

var (
	digestImagePattern  = regexp.MustCompile(`^[^\s@]+@(sha256:[a-f0-9]{64})$`)
	agnhostImagePattern = regexp.MustCompile(`^(?:[^\s/@]+/)*agnhost(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?@sha256:[a-f0-9]{64}$`)
	gitRevisionPattern  = regexp.MustCompile(`^[a-f0-9]{40}$`)
)

// ProjectMode identifies how exclusively the E2E run owns its OpenStack project.
type ProjectMode string

// File is the strict user-facing E2E configuration document.
type File struct {
	FormatVersion string     `json:"formatVersion"`
	RunID         string     `json:"runID,omitempty"`
	Kubernetes    Kubernetes `json:"kubernetes"`
	OpenStack     OpenStack  `json:"openstack"`
	Controller    Controller `json:"controller"`
	Backend       Backend    `json:"backend"`
	Artifacts     Artifacts  `json:"artifacts,omitempty"`
}

// Kubernetes selects the cluster used by the run.
type Kubernetes struct {
	DedicatedForE2E bool   `json:"dedicatedForE2E"`
	Kubeconfig      string `json:"kubeconfig"`
	Context         string `json:"context"`
}

// OpenStack selects the project, credentials, and networks used by the run.
type OpenStack struct {
	ProjectMode                     ProjectMode `json:"projectMode"`
	DedicatedForE2E                 bool        `json:"dedicatedForE2E,omitempty"`
	AcceptProjectWideCredentialRisk bool        `json:"acceptProjectWideCredentialRisk,omitempty"`
	ExpectedProjectID               string      `json:"expectedProjectID"`
	Cloud                           string      `json:"cloud"`
	Region                          string      `json:"region"`
	ControllerCloudsYAML            string      `json:"controllerCloudsYAML"`
	AuditCloudsYAML                 string      `json:"auditCloudsYAML,omitempty"`
	VIPSubnetID                     string      `json:"vipSubnetID"`
	MemberSubnetID                  string      `json:"memberSubnetID"`
	ExternalNetworkID               string      `json:"externalNetworkID"`
}

// Controller identifies the immutable controller artifact used by the run.
type Controller struct {
	Domain         string `json:"domain"`
	Image          string `json:"image"`
	SourceRevision string `json:"sourceRevision"`
}

// Backend identifies the immutable backend artifact used by the run.
type Backend struct {
	Image string `json:"image"`
}

// Artifacts selects the parent directory for run evidence.
type Artifacts struct {
	Root string `json:"root,omitempty"`
}

// ResolveOptions provides process-local inputs that are not part of the user file.
type ResolveOptions struct {
	RepositoryRoot string
	Now            func() time.Time
	Random         io.Reader
}

type resolvedIdentity struct {
	runID          string
	identity       string
	controllerName string
}

type resolvedKubernetes struct {
	kubeconfig string
	context    string
}

type resolvedArtifacts struct {
	controllerImage       string
	controllerImageDigest string
	controllerRevision    string
	backendImage          string
	controllerCloudsYAML  string
	auditCloudsYAML       string
	auditBinary           string
	artifactsRoot         string
}

// Load reads a strict YAML configuration from an absolute path.
func Load(path string) (File, error) {
	if path == "" || !filepath.IsAbs(path) {
		return File{}, fmt.Errorf("e2e config path must be absolute")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return File{}, fmt.Errorf("read e2e config: %w", err)
	}
	var config File
	if err := yaml.UnmarshalStrict(contents, &config); err != nil {
		return File{}, fmt.Errorf("decode e2e config: %w", err)
	}
	return config, nil
}

// Resolve validates a user configuration and derives one immutable run identity.
func Resolve(config File, options ResolveOptions) (Runtime, error) {
	options, err := normalizeResolveOptions(options)
	if err != nil {
		return Runtime{}, err
	}
	if err := validateFilePolicy(config); err != nil {
		return Runtime{}, err
	}
	identity, err := resolveIdentity(config, options)
	if err != nil {
		return Runtime{}, err
	}
	kubernetes, err := resolveKubernetes(config.Kubernetes)
	if err != nil {
		return Runtime{}, err
	}
	artifacts, err := resolveArtifacts(config, options.RepositoryRoot)
	if err != nil {
		return Runtime{}, err
	}
	externalNetworkID := strings.TrimSpace(config.OpenStack.ExternalNetworkID)
	if externalNetworkID == disabledNetwork {
		externalNetworkID = ""
	}
	runtime := newRuntime(config, identity, kubernetes, artifacts, externalNetworkID)
	if err := runtime.Validate(); err != nil {
		return Runtime{}, fmt.Errorf("validate resolved runtime: %w", err)
	}
	return runtime, nil
}

func newRuntime(
	config File,
	identity resolvedIdentity,
	kubernetes resolvedKubernetes,
	artifacts resolvedArtifacts,
	externalNetworkID string,
) Runtime {
	return Runtime{
		FormatVersion:          FormatVersion,
		RunID:                  identity.runID,
		Namespace:              workloadNamespacePrefix + identity.runID,
		Kubeconfig:             kubernetes.kubeconfig,
		KubeContext:            kubernetes.context,
		ControllerName:         identity.controllerName,
		ControllerNamespace:    controllerNamespacePrefix + identity.runID,
		ControllerDeployment:   controllerDeploymentName,
		ControllerContainer:    controllerContainerName,
		ControllerReplicas:     controllerReplicas,
		ControllerImage:        artifacts.controllerImage,
		ControllerImageDigest:  artifacts.controllerImageDigest,
		ControllerRevision:     artifacts.controllerRevision,
		ControllerCloudsYAML:   artifacts.controllerCloudsYAML,
		LeaderLease:            controllerLeaseName,
		BackendImage:           artifacts.backendImage,
		ArtifactDirectory:      filepath.Join(filepath.Clean(artifacts.artifactsRoot), identity.runID),
		RestartMode:            defaultRestartMode,
		Timeout:                defaultTimeout,
		PollInterval:           defaultPollInterval,
		HTTPTimeout:            defaultHTTPTimeout,
		NoOpWindow:             defaultNoOpWindow,
		GatewayClassName:       identity.identity,
		ClusterRoleName:        identity.identity + "-controller",
		ClusterRoleBindingName: identity.identity + "-controller",
		Project: Project{
			Mode:                      config.OpenStack.ProjectMode,
			ExpectedProjectID:         strings.TrimSpace(config.OpenStack.ExpectedProjectID),
			ExpectedVIPSubnetID:       strings.TrimSpace(config.OpenStack.VIPSubnetID),
			ExpectedMemberSubnetID:    strings.TrimSpace(config.OpenStack.MemberSubnetID),
			ExpectedExternalNetworkID: externalNetworkID,
			ControllerCloudsSecret:    controllerSecretName,
		},
		Audit: Audit{
			Binary:       artifacts.auditBinary,
			ClusterID:    identity.identity,
			CloudsYAML:   artifacts.auditCloudsYAML,
			Cloud:        strings.TrimSpace(config.OpenStack.Cloud),
			Region:       strings.TrimSpace(config.OpenStack.Region),
			Microversion: octaviaMicroversion,
		},
	}
}

func normalizeResolveOptions(options ResolveOptions) (ResolveOptions, error) {
	if options.RepositoryRoot == "" || !filepath.IsAbs(options.RepositoryRoot) {
		return ResolveOptions{}, fmt.Errorf("repository root must be absolute")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Random == nil {
		options.Random = rand.Reader
	}
	return options, nil
}

func validateFilePolicy(config File) error {
	if config.FormatVersion != FormatVersion {
		return fmt.Errorf("formatVersion must be exactly %q", FormatVersion)
	}
	if !config.Kubernetes.DedicatedForE2E {
		return fmt.Errorf("kubernetes.dedicatedForE2E must be exactly true")
	}
	if err := validateProjectAcknowledgement(config.OpenStack); err != nil {
		return err
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
			return fmt.Errorf("%s must not be empty", item.name)
		}
	}
	return nil
}

func resolveIdentity(config File, options ResolveOptions) (resolvedIdentity, error) {
	runID := strings.TrimSpace(config.RunID)
	if runID == "" {
		generated, err := generateRunID(options.Now(), options.Random)
		if err != nil {
			return resolvedIdentity{}, err
		}
		runID = generated
	}
	if len(runID) < 8 || len(runID) > 32 || len(validation.IsDNS1123Label(runID)) != 0 {
		return resolvedIdentity{}, fmt.Errorf("runID must be a DNS label between 8 and 32 characters")
	}
	domain := strings.TrimSpace(config.Controller.Domain)
	if len(validation.IsDNS1123Subdomain(domain)) != 0 {
		return resolvedIdentity{}, fmt.Errorf("controller.domain must be a DNS subdomain")
	}
	identity := runIdentityPrefix + runID
	controllerName := domain + "/" + identity
	if len(controllerName) > 253 {
		return resolvedIdentity{}, fmt.Errorf("derived controller name must not exceed 253 characters")
	}
	return resolvedIdentity{runID: runID, identity: identity, controllerName: controllerName}, nil
}

func resolveKubernetes(config Kubernetes) (resolvedKubernetes, error) {
	kubeconfig := strings.TrimSpace(config.Kubeconfig)
	if kubeconfig == "" || !filepath.IsAbs(kubeconfig) {
		return resolvedKubernetes{}, fmt.Errorf("kubernetes.kubeconfig must be an absolute path")
	}
	context := strings.TrimSpace(config.Context)
	if context == "" {
		return resolvedKubernetes{}, fmt.Errorf("kubernetes.context must not be empty")
	}
	return resolvedKubernetes{kubeconfig: kubeconfig, context: context}, nil
}

func resolveArtifacts(config File, repositoryRoot string) (resolvedArtifacts, error) {
	image := strings.TrimSpace(config.Controller.Image)
	imageMatch := digestImagePattern.FindStringSubmatch(image)
	if len(imageMatch) != 2 {
		return resolvedArtifacts{}, fmt.Errorf("controller.image must be pinned by a lowercase sha256 digest")
	}
	revision := strings.TrimSpace(config.Controller.SourceRevision)
	if !gitRevisionPattern.MatchString(revision) {
		return resolvedArtifacts{}, fmt.Errorf("controller.sourceRevision must be a full 40-character lowercase Git commit")
	}
	backendImage := strings.TrimSpace(config.Backend.Image)
	if !agnhostImagePattern.MatchString(backendImage) {
		return resolvedArtifacts{}, fmt.Errorf("backend.image must be a digest-pinned agnhost image")
	}
	controllerCloudsYAML, auditCloudsYAML, err := resolveCloudPaths(config.OpenStack)
	if err != nil {
		return resolvedArtifacts{}, err
	}
	artifactsRoot := strings.TrimSpace(config.Artifacts.Root)
	if artifactsRoot == "" {
		artifactsRoot = filepath.Join(repositoryRoot, "_artifacts", "e2e")
	}
	if !filepath.IsAbs(artifactsRoot) || filepath.Clean(artifactsRoot) == string(filepath.Separator) {
		return resolvedArtifacts{}, fmt.Errorf("artifacts.root must be a non-root absolute path")
	}
	return resolvedArtifacts{
		controllerImage:       image,
		controllerImageDigest: imageMatch[1],
		controllerRevision:    revision,
		backendImage:          backendImage,
		controllerCloudsYAML:  controllerCloudsYAML,
		auditCloudsYAML:       auditCloudsYAML,
		auditBinary:           filepath.Join(repositoryRoot, "bin", "openstack-gateway-audit"),
		artifactsRoot:         artifactsRoot,
	}, nil
}

func resolveCloudPaths(config OpenStack) (string, string, error) {
	controller := strings.TrimSpace(config.ControllerCloudsYAML)
	if controller == "" || !filepath.IsAbs(controller) {
		return "", "", fmt.Errorf("openstack.controllerCloudsYAML must be an absolute path")
	}
	audit := strings.TrimSpace(config.AuditCloudsYAML)
	if audit == "" {
		audit = controller
	}
	if !filepath.IsAbs(audit) {
		return "", "", fmt.Errorf("openstack.auditCloudsYAML must be an absolute path")
	}
	return controller, audit, nil
}

func validateProjectAcknowledgement(config OpenStack) error {
	switch config.ProjectMode {
	case ProjectModeDedicated:
		if !config.DedicatedForE2E {
			return fmt.Errorf("openstack.dedicatedForE2E must be exactly true in dedicated mode")
		}
		if config.AcceptProjectWideCredentialRisk {
			return fmt.Errorf("openstack.acceptProjectWideCredentialRisk must be false or omitted in dedicated mode")
		}
	case ProjectModeShared:
		if config.DedicatedForE2E {
			return fmt.Errorf("openstack.dedicatedForE2E must be false or omitted in shared mode")
		}
		if !config.AcceptProjectWideCredentialRisk {
			return fmt.Errorf("openstack.acceptProjectWideCredentialRisk must be exactly true in shared mode")
		}
	default:
		return fmt.Errorf("openstack.projectMode must be exactly %q or %q", ProjectModeDedicated, ProjectModeShared)
	}
	return nil
}

func generateRunID(now time.Time, random io.Reader) (string, error) {
	suffix := make([]byte, 4)
	if _, err := io.ReadFull(random, suffix); err != nil {
		return "", fmt.Errorf("generate e2e run ID: %w", err)
	}
	return fmt.Sprintf("run-%s-%x", now.UTC().Format("20060102-150405"), suffix), nil
}
