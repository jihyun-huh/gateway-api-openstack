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

package runconfig

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

const maximumRuntimeBytes = 1 << 20

// Runtime is the validated, non-secret configuration passed to the live test process.
type Runtime struct {
	FormatVersion          string        `json:"formatVersion"`
	RunID                  string        `json:"runID"`
	Namespace              string        `json:"namespace"`
	Kubeconfig             string        `json:"kubeconfig"`
	KubeContext            string        `json:"kubeContext"`
	ControllerName         string        `json:"controllerName"`
	ControllerNamespace    string        `json:"controllerNamespace"`
	ControllerDeployment   string        `json:"controllerDeployment"`
	ControllerContainer    string        `json:"controllerContainer"`
	ControllerReplicas     int32         `json:"controllerReplicas"`
	ControllerImage        string        `json:"controllerImage"`
	ControllerImageDigest  string        `json:"controllerImageDigest"`
	ControllerRevision     string        `json:"controllerRevision"`
	ControllerCloudsYAML   string        `json:"controllerCloudsYAML"`
	LeaderLease            string        `json:"leaderLease"`
	BackendImage           string        `json:"backendImage"`
	ArtifactDirectory      string        `json:"artifactDirectory"`
	RestartMode            string        `json:"restartMode"`
	Timeout                time.Duration `json:"timeout"`
	PollInterval           time.Duration `json:"pollInterval"`
	HTTPTimeout            time.Duration `json:"httpTimeout"`
	NoOpWindow             time.Duration `json:"noOpWindow"`
	GatewayClassName       string        `json:"gatewayClassName"`
	ClusterRoleName        string        `json:"clusterRoleName"`
	ClusterRoleBindingName string        `json:"clusterRoleBindingName"`
	Project                Project       `json:"project"`
	Audit                  Audit         `json:"audit"`
}

// Project is the resolved OpenStack project and network contract.
type Project struct {
	Mode                      ProjectMode `json:"mode"`
	ExpectedProjectID         string      `json:"expectedProjectID"`
	ExpectedVIPSubnetID       string      `json:"expectedVIPSubnetID"`
	ExpectedMemberSubnetID    string      `json:"expectedMemberSubnetID"`
	ExpectedExternalNetworkID string      `json:"expectedExternalNetworkID"`
	ControllerCloudsSecret    string      `json:"controllerCloudsSecret"`
}

// Audit is the required ownership audit configuration for the run.
type Audit struct {
	Binary       string `json:"binary"`
	ClusterID    string `json:"clusterID"`
	CloudsYAML   string `json:"cloudsYAML"`
	Cloud        string `json:"cloud"`
	Region       string `json:"region"`
	Microversion string `json:"microversion"`
}

// Validate verifies that a runtime document retains every derived safety invariant.
func (c Runtime) Validate() error {
	if c.FormatVersion != FormatVersion {
		return fmt.Errorf("runtime formatVersion must be exactly %q", FormatVersion)
	}
	for _, validate := range []func() error{
		c.validateIdentity,
		c.validateKubernetes,
		c.validateArtifacts,
		c.validateOpenStack,
		c.validateExecution,
	} {
		if err := validate(); err != nil {
			return err
		}
	}
	return nil
}

func (c Runtime) validateIdentity() error {
	if c.Project.Mode != ProjectModeDedicated && c.Project.Mode != ProjectModeShared {
		return fmt.Errorf("runtime project mode is invalid")
	}
	if len(c.RunID) < 8 || len(c.RunID) > 32 || len(validation.IsDNS1123Label(c.RunID)) != 0 {
		return fmt.Errorf("runtime run ID is invalid")
	}
	if err := c.validateDerivedNames(); err != nil {
		return err
	}
	return c.validateControllerName()
}

func (c Runtime) validateDerivedNames() error {
	identity := runIdentityPrefix + c.RunID
	want := []struct {
		actual   string
		expected string
	}{
		{actual: c.Namespace, expected: workloadNamespacePrefix + c.RunID},
		{actual: c.ControllerNamespace, expected: controllerNamespacePrefix + c.RunID},
		{actual: c.GatewayClassName, expected: identity},
		{actual: c.Audit.ClusterID, expected: identity},
		{actual: c.ClusterRoleName, expected: identity + "-controller"},
		{actual: c.ClusterRoleBindingName, expected: identity + "-controller"},
	}
	for _, item := range want {
		if item.actual != item.expected {
			return fmt.Errorf("runtime identity is inconsistent")
		}
	}
	return nil
}

func (c Runtime) validateControllerName() error {
	identity := runIdentityPrefix + c.RunID
	_, controllerPath, found := strings.Cut(c.ControllerName, "/")
	if !found || controllerPath != identity {
		return fmt.Errorf("runtime identity is inconsistent")
	}
	if len(validation.IsDomainPrefixedPath(field.NewPath("controllerName"), c.ControllerName)) != 0 {
		return fmt.Errorf("runtime controller name is inconsistent")
	}
	return nil
}

func (c Runtime) validateKubernetes() error {
	if c.Kubeconfig == "" || !filepath.IsAbs(c.Kubeconfig) || c.KubeContext == "" {
		return fmt.Errorf("runtime Kubernetes selection is incomplete")
	}
	return nil
}

func (c Runtime) validateArtifacts() error {
	if c.ControllerDeployment != controllerDeploymentName || c.ControllerContainer != controllerContainerName ||
		c.ControllerReplicas != controllerReplicas || c.LeaderLease != controllerLeaseName {
		return fmt.Errorf("runtime controller deployment contract is invalid")
	}
	imageMatch := digestImagePattern.FindStringSubmatch(c.ControllerImage)
	if len(imageMatch) != 2 || imageMatch[1] != c.ControllerImageDigest || !gitRevisionPattern.MatchString(c.ControllerRevision) {
		return fmt.Errorf("runtime controller artifact is invalid")
	}
	if !agnhostImagePattern.MatchString(c.BackendImage) {
		return fmt.Errorf("runtime backend artifact is invalid")
	}
	return nil
}

func (c Runtime) validateOpenStack() error {
	for _, item := range []struct {
		name  string
		value string
	}{
		{name: "expected project ID", value: c.Project.ExpectedProjectID},
		{name: "expected VIP subnet ID", value: c.Project.ExpectedVIPSubnetID},
		{name: "expected member subnet ID", value: c.Project.ExpectedMemberSubnetID},
		{name: "controller clouds.yaml", value: c.ControllerCloudsYAML},
		{name: "audit binary", value: c.Audit.Binary},
		{name: "audit clouds.yaml", value: c.Audit.CloudsYAML},
		{name: "audit cloud", value: c.Audit.Cloud},
		{name: "audit region", value: c.Audit.Region},
		{name: "audit microversion", value: c.Audit.Microversion},
	} {
		if strings.TrimSpace(item.value) == "" {
			return fmt.Errorf("runtime %s must not be empty", item.name)
		}
	}
	for _, path := range []string{c.ControllerCloudsYAML, c.Audit.Binary, c.Audit.CloudsYAML, c.ArtifactDirectory} {
		if !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
			return fmt.Errorf("runtime paths must be absolute and non-root")
		}
	}
	if filepath.Base(filepath.Clean(c.ArtifactDirectory)) != c.RunID {
		return fmt.Errorf("runtime artifact directory must end with the run ID")
	}
	if c.Project.ControllerCloudsSecret != controllerSecretName {
		return fmt.Errorf("runtime controller clouds Secret is invalid")
	}
	return nil
}

func (c Runtime) validateExecution() error {
	if c.RestartMode != defaultRestartMode || c.Timeout != defaultTimeout || c.PollInterval != defaultPollInterval ||
		c.HTTPTimeout != defaultHTTPTimeout || c.NoOpWindow != defaultNoOpWindow {
		return fmt.Errorf("runtime timing contract is invalid")
	}
	return nil
}

// WriteRuntime writes a validated runtime document to a new private file in directory.
func WriteRuntime(directory string, config Runtime) (string, error) {
	if directory == "" || !filepath.IsAbs(directory) {
		return "", fmt.Errorf("runtime directory must be absolute")
	}
	if err := config.Validate(); err != nil {
		return "", err
	}
	contents, err := json.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("encode runtime config: %w", err)
	}
	contents = append(contents, '\n')
	file, err := os.CreateTemp(directory, "gateway-api-openstack-e2e-runtime-*.json")
	if err != nil {
		return "", fmt.Errorf("create runtime config: %w", err)
	}
	path := file.Name()
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return "", fmt.Errorf("make runtime config private: %w", err)
	}
	if _, err := file.Write(contents); err != nil {
		return "", fmt.Errorf("write runtime config: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close runtime config: %w", err)
	}
	remove = false
	return path, nil
}

// LoadRuntime reads and validates a strict private runtime JSON document.
func LoadRuntime(path string) (Runtime, error) {
	file, err := openRuntimeFile(path)
	if err != nil {
		return Runtime{}, err
	}
	defer func() { _ = file.Close() }()
	config, err := decodeRuntime(file)
	if err != nil {
		return Runtime{}, err
	}
	if err := config.Validate(); err != nil {
		return Runtime{}, fmt.Errorf("validate runtime config: %w", err)
	}
	return config, nil
}

func openRuntimeFile(path string) (*os.File, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("runtime config path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect runtime config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("runtime config must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("runtime config must not be accessible by group or other users")
	}
	if info.Size() > maximumRuntimeBytes {
		return nil, fmt.Errorf("runtime config exceeds the size limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open runtime config: %w", err)
	}
	return file, nil
}

func decodeRuntime(reader io.Reader) (Runtime, error) {
	decoder := json.NewDecoder(io.LimitReader(reader, maximumRuntimeBytes+1))
	decoder.DisallowUnknownFields()
	var config Runtime
	if err := decoder.Decode(&config); err != nil {
		return Runtime{}, fmt.Errorf("decode runtime config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Runtime{}, fmt.Errorf("decode runtime config: unexpected trailing JSON value")
		}
		return Runtime{}, fmt.Errorf("decode runtime config: %w", err)
	}
	return config, nil
}
