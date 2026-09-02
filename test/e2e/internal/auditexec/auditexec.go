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

// Package auditexec runs the E2E ownership audit with bounded output and
// validates the report before a caller uses it as test evidence.
package auditexec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/jihyun-huh/gateway-api-openstack/internal/audit"
)

const defaultOutputLimit = 8 << 20

// CommandRunner runs one audit process. Tests may replace it without starting
// an external process.
type CommandRunner func(ctx context.Context, binary string, arguments, environment []string, stdout, stderr io.Writer) error

// Config contains the exact ownership scope and command settings for one audit
// invocation.
type Config struct {
	Binary         string
	ControllerName string
	ClusterID      string
	Kubeconfig     string
	Context        string
	Microversion   string
	CloudsYAML     string
	Cloud          string
	Region         string
	Environment    []string
	OutputLimit    int
	RunCommand     CommandRunner
}

// Validation controls the report state accepted by Run and ValidateReport.
type Validation struct {
	RequireEmpty bool
}

// Run executes an ownership audit, bounds both output streams, decodes stdout,
// and validates the returned report.
func Run(ctx context.Context, config Config, validation Validation) (audit.Report, error) {
	arguments, err := Arguments(config)
	if err != nil {
		return audit.Report{}, err
	}
	limit := config.OutputLimit
	if limit == 0 {
		limit = defaultOutputLimit
	}
	if limit < 0 {
		return audit.Report{}, fmt.Errorf("ownership audit output limit must not be negative")
	}
	stdout := boundedBuffer{limit: limit}
	stderr := boundedBuffer{limit: limit}
	runCommand := config.RunCommand
	if runCommand == nil {
		runCommand = runProcess
	}
	runErr := runCommand(ctx, config.Binary, arguments, config.Environment, &stdout, &stderr)
	if stdout.overflow || stderr.overflow {
		return audit.Report{}, fmt.Errorf("ownership audit output exceeded the safe in-memory limit")
	}
	if runErr != nil {
		return audit.Report{}, fmt.Errorf("ownership audit command failed")
	}

	var report audit.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		return audit.Report{}, fmt.Errorf("ownership audit output was not valid JSON")
	}
	if err := ValidateReport(report, config.ControllerName, config.ClusterID, validation); err != nil {
		return audit.Report{}, err
	}
	return report, nil
}

// Arguments returns the deterministic arguments for an ownership audit.
func Arguments(config Config) ([]string, error) {
	for _, value := range []string{config.Binary, config.ControllerName, config.ClusterID, config.Kubeconfig, config.Context, config.Microversion} {
		if value == "" || strings.TrimSpace(value) != value {
			return nil, fmt.Errorf("ownership audit command configuration is incomplete")
		}
	}
	if config.CloudsYAML != "" && config.Cloud == "" {
		return nil, fmt.Errorf("ownership audit cloud must accompany clouds.yaml")
	}
	arguments := []string{
		"--controller-name=" + config.ControllerName,
		"--cluster-id=" + config.ClusterID,
		"--kubeconfig=" + config.Kubeconfig,
		"--context=" + config.Context,
		"--octavia-microversion=" + config.Microversion,
	}
	if config.CloudsYAML != "" {
		arguments = append(arguments, "--clouds-yaml="+config.CloudsYAML)
	}
	if config.Cloud != "" {
		arguments = append(arguments, "--openstack-cloud="+config.Cloud)
	}
	if config.Region != "" {
		arguments = append(arguments, "--openstack-region="+config.Region)
	}
	return arguments, nil
}

// ValidateReport requires a supported complete report for the exact scope with
// no unresolved findings. It optionally requires the scope to be empty.
func ValidateReport(report audit.Report, controllerName, clusterID string, validation Validation) error {
	if err := validateReportHeader(report, controllerName, clusterID); err != nil {
		return err
	}
	if err := validateNoFindings(report); err != nil {
		return err
	}
	if validation.RequireEmpty && !reportIsEmpty(report) {
		return fmt.Errorf("ownership audit scope was not empty")
	}
	return nil
}

func validateReportHeader(report audit.Report, controllerName, clusterID string) error {
	if controllerName == "" || clusterID == "" {
		return fmt.Errorf("ownership audit expected scope is incomplete")
	}
	if report.FormatVersion != audit.ReportFormatVersion || report.Assessment != audit.AssessmentComplete {
		return fmt.Errorf("ownership audit did not produce a complete supported report")
	}
	if report.Scope.ControllerName != controllerName || report.Scope.ClusterID != clusterID {
		return fmt.Errorf("ownership audit report scope did not match the configured run")
	}
	return nil
}

func validateNoFindings(report audit.Report) error {
	if len(report.KubernetesIssues) != 0 || report.HasFindings() {
		return fmt.Errorf("ownership audit reported unresolved findings")
	}
	if hasUnmatchedResources(report.Resources) {
		return fmt.Errorf("ownership audit reported unresolved findings")
	}
	return nil
}

func reportIsEmpty(report audit.Report) bool {
	return report.Summary == (audit.Summary{}) && len(report.Resources) == 0
}

func hasUnmatchedResources(resources []audit.ResourceFinding) bool {
	for _, resource := range resources {
		if resource.Disposition != audit.DispositionMatched {
			return true
		}
	}
	return false
}

func runProcess(ctx context.Context, binary string, arguments, environment []string, stdout, stderr io.Writer) error {
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Env = environment
	command.Stdout = stdout
	command.Stderr = stderr
	return command.Run()
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(contents []byte) (int, error) {
	written := len(contents)
	remaining := b.limit - b.buffer.Len()
	if remaining < len(contents) {
		b.overflow = true
	}
	if remaining <= 0 {
		return written, nil
	}
	if len(contents) > remaining {
		contents = contents[:remaining]
	}
	_, _ = b.buffer.Write(contents)
	return written, nil
}

func (b *boundedBuffer) Bytes() []byte { return b.buffer.Bytes() }
