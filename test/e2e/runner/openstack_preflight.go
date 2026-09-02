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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	openstackclouds "github.com/gophercloud/gophercloud/v2/openstack/config/clouds"
	"sigs.k8s.io/yaml"

	"github.com/jihyun-huh/gateway-api-openstack/internal/audit"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack"
)

const maximumPreflightAuditOutputBytes = 8 << 20

type projectAuthenticator func(context.Context, string, string, string) (string, error)
type ownershipAuditRunner func(context.Context, resolvedConfig, []string) error

func preflightOpenStack(
	ctx context.Context,
	config resolvedConfig,
	controllerCloudsYAML []byte,
	authenticate projectAuthenticator,
	runAudit ownershipAuditRunner,
) error {
	if err := validateControllerCloudsYAML(controllerCloudsYAML, config.cloud); err != nil {
		return err
	}
	if authenticate == nil {
		authenticate = authenticateProject
	}
	if runAudit == nil {
		runAudit = runEmptyOwnershipAudit
	}

	controllerPath, cleanup, err := writeExactCloudsYAML(controllerCloudsYAML)
	if err != nil {
		return err
	}
	defer cleanup()

	controllerProject, err := authenticate(ctx, controllerPath, config.cloud, config.region)
	if err != nil || controllerProject == "" || controllerProject != config.expectedProjectID {
		return fmt.Errorf("controller credential did not authenticate to the approved shared project")
	}
	auditProject, err := authenticate(ctx, config.auditCloudsYAML, config.cloud, config.region)
	if err != nil || auditProject == "" || auditProject != config.expectedProjectID {
		return fmt.Errorf("audit credential did not authenticate to the approved shared project")
	}
	if err := runAudit(ctx, config, mergeHarnessEnvironment(os.Environ(), nil)); err != nil {
		return err
	}
	return nil
}

func authenticateProject(ctx context.Context, cloudsYAMLPath string, cloudName string, region string) (string, error) {
	clients, err := openstack.NewServiceClients(ctx, openstack.ClientConfig{
		Region:         region,
		Microversion:   octaviaMicroversion,
		CloudsYAMLPath: cloudsYAMLPath,
		CloudName:      cloudName,
		APIQPS:         openstack.DefaultAPIQPS,
		APIBurst:       openstack.DefaultAPIBurst,
	})
	if err != nil {
		return "", err
	}
	return clients.ProjectID, nil
}

func validateControllerCloudsYAML(contents []byte, cloudName string) error {
	var document openstackclouds.Clouds
	if err := yaml.Unmarshal(contents, &document); err != nil {
		return fmt.Errorf("controller clouds.yaml could not be parsed")
	}
	cloud, found := document.Clouds[cloudName]
	if len(document.Clouds) != 1 || !found || cloud.AuthInfo == nil || cloud.Profile != "" || cloud.Cloud != "" || cloud.Verify != nil && !*cloud.Verify {
		return fmt.Errorf("controller clouds.yaml must contain one self-contained verified cloud entry")
	}
	if cloud.CACertFile != "" || cloud.ClientCertFile != "" || cloud.ClientKeyFile != "" {
		return fmt.Errorf("the quick runner does not support auxiliary TLS files in controller clouds.yaml")
	}
	if cloud.AuthType != "" && cloud.AuthType != openstackclouds.AuthV3ApplicationCredential {
		return fmt.Errorf("controller clouds.yaml must use an application credential")
	}
	auth := cloud.AuthInfo
	identities := 0
	if auth.ApplicationCredentialID != "" {
		identities++
	}
	if auth.ApplicationCredentialName != "" {
		identities++
	}
	if auth.AuthURL == "" || identities != 1 || auth.ApplicationCredentialSecret == "" ||
		auth.Password != "" || auth.Token != "" || auth.SystemScope != "" || auth.TrustID != "" {
		return fmt.Errorf("controller clouds.yaml does not satisfy the application credential contract")
	}
	if auth.ApplicationCredentialName != "" && auth.UserID == "" && auth.Username == "" {
		return fmt.Errorf("named application credential does not identify its user")
	}
	return nil
}

func writeExactCloudsYAML(contents []byte) (string, func(), error) {
	directory, err := os.MkdirTemp("", "gateway-api-openstack-e2e-auth-")
	if err != nil {
		return "", func() {}, fmt.Errorf("prepare controller credential verification")
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	path := filepath.Join(directory, "clouds.yaml")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("prepare controller credential verification")
	}
	return path, cleanup, nil
}

func runEmptyOwnershipAudit(ctx context.Context, config resolvedConfig, environment []string) error {
	arguments := []string{
		"--controller-name=" + config.controllerName,
		"--cluster-id=" + config.clusterID,
		"--kubeconfig=" + config.kubeconfig,
		"--context=" + config.kubeContext,
		"--octavia-microversion=" + octaviaMicroversion,
		"--clouds-yaml=" + config.auditCloudsYAML,
		"--openstack-cloud=" + config.cloud,
		"--openstack-region=" + config.region,
	}
	command := exec.CommandContext(ctx, config.auditBinary, arguments...)
	command.Env = environment
	stdout := boundedBuffer{limit: maximumPreflightAuditOutputBytes}
	stderr := boundedBuffer{limit: maximumPreflightAuditOutputBytes}
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("pre-install ownership audit failed")
	}
	if stdout.overflow || stderr.overflow {
		return fmt.Errorf("pre-install ownership audit output exceeded the safe in-memory limit")
	}
	var report audit.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		return fmt.Errorf("pre-install ownership audit output was not valid JSON")
	}
	if err := validateEmptyOwnershipAudit(report, config); err != nil {
		return err
	}
	return nil
}

func validateEmptyOwnershipAudit(report audit.Report, config resolvedConfig) error {
	if report.FormatVersion != audit.ReportFormatVersion || report.Assessment != audit.AssessmentComplete {
		return fmt.Errorf("pre-install ownership audit did not produce a complete supported report")
	}
	if report.Scope.ControllerName != config.controllerName || report.Scope.ClusterID != config.clusterID {
		return fmt.Errorf("pre-install ownership audit scope did not match the run")
	}
	if report.Summary != (audit.Summary{}) || len(report.KubernetesIssues) != 0 || len(report.Resources) != 0 || report.HasFindings() {
		return fmt.Errorf("pre-install ownership scope is not empty")
	}
	return nil
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(contents []byte) (int, error) {
	written := len(contents)
	remaining := b.limit + 1 - b.buffer.Len()
	if remaining > 0 {
		if len(contents) > remaining {
			contents = contents[:remaining]
		}
		_, _ = b.buffer.Write(contents)
	}
	if b.buffer.Len() > b.limit {
		b.overflow = true
	}
	return written, nil
}

func (b *boundedBuffer) Bytes() []byte { return b.buffer.Bytes() }
