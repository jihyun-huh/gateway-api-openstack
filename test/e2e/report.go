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
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const reportFormatVersion = "v1alpha1"

const (
	checkSummaryPassed                = "The check completed with the expected result"
	checkSummaryFailed                = "The check did not reach the expected result; details are omitted"
	checkSummaryEvidenceNotConfigured = "Required evidence input was not configured"
	checkSummaryAuditNotConfigured    = "Ownership audit configuration was not supplied"
	checkSummaryFaultNotConfigured    = "No safe fault injector was configured for this run"
)

type checkStatus string

const (
	statusPassed  checkStatus = "Passed"
	statusFailed  checkStatus = "Failed"
	statusSkipped checkStatus = "Skipped"
	statusNotRun  checkStatus = "Not run"
)

var orderedCheckNames = []string{
	"preflight safety validation",
	"isolated NodePort backend",
	"GatewayClass status",
	"Gateway status",
	"HTTPRoute status",
	"active ownership inventory audit",
	"external HTTP traffic",
	"leader pod deletion and recovery",
	"cold controller restart and recovery",
	"converged metrics snapshot",
	"orderly deletion and finalizer completion",
	"post-test ownership audit returns to baseline",
	"external deletion of an owned child resource",
	"blocked finalization",
	"quota failure",
	"request timeout and rate limiting",
	"Octavia resource failure",
}

var requiredBaselineChecks = map[string]struct{}{
	"preflight safety validation":                   {},
	"isolated NodePort backend":                     {},
	"GatewayClass status":                           {},
	"Gateway status":                                {},
	"HTTPRoute status":                              {},
	"active ownership inventory audit":              {},
	"external HTTP traffic":                         {},
	"leader pod deletion and recovery":              {},
	"cold controller restart and recovery":          {},
	"converged metrics snapshot":                    {},
	"orderly deletion and finalizer completion":     {},
	"post-test ownership audit returns to baseline": {},
}

type checkResult struct {
	Name    string      `json:"name"`
	Status  checkStatus `json:"status"`
	Summary string      `json:"summary"`
}

type auditSummary struct {
	Assessment       string `json:"assessment"`
	Bindings         int    `json:"bindings"`
	KubernetesIssues int    `json:"kubernetesIssues"`
	Resources        int    `json:"resources"`
	Matched          int    `json:"matched"`
	OrphanCandidates int    `json:"orphanCandidates"`
	StaleUIDs        int    `json:"staleUIDs"`
	Conflicts        int    `json:"ownershipConflicts"`
	Unresolved       int    `json:"unresolved"`
}

type auditEvidence struct {
	Baseline     *auditSummary `json:"baseline,omitempty"`
	Active       *auditSummary `json:"active,omitempty"`
	AfterLeader  *auditSummary `json:"afterLeaderRecovery,omitempty"`
	AfterRestart *auditSummary `json:"afterColdRestart,omitempty"`
	AfterCleanup *auditSummary `json:"afterCleanup,omitempty"`
}

type metricsEvidence struct {
	Before map[string]float64 `json:"before,omitempty"`
	After  map[string]float64 `json:"after,omitempty"`
}

type e2eReport struct {
	FormatVersion         string           `json:"formatVersion"`
	Suite                 string           `json:"suite"`
	Status                checkStatus      `json:"status"`
	StartedAt             time.Time        `json:"startedAt"`
	CompletedAt           time.Time        `json:"completedAt"`
	GatewayAPIBundle      string           `json:"gatewayAPIBundle"`
	ControllerRevision    string           `json:"controllerRevision"`
	ControllerImageDigest string           `json:"controllerImageDigest"`
	ProjectMode           projectMode      `json:"projectMode"`
	RestartMode           string           `json:"restartMode"`
	Checks                []checkResult    `json:"checks"`
	Audit                 *auditEvidence   `json:"audit,omitempty"`
	Metrics               *metricsEvidence `json:"metrics,omitempty"`
}

func newE2EReport(startedAt time.Time, projectMode projectMode, restartMode, controllerRevision, controllerImageDigest string) *e2eReport {
	report := &e2eReport{
		FormatVersion:         reportFormatVersion,
		Suite:                 "Phase 2 E2E foundations",
		Status:                statusNotRun,
		StartedAt:             startedAt.UTC(),
		GatewayAPIBundle:      "v1.6.1 Standard Channel",
		ControllerRevision:    controllerRevision,
		ControllerImageDigest: controllerImageDigest,
		ProjectMode:           projectMode,
		RestartMode:           restartMode,
		Checks:                make([]checkResult, 0, len(orderedCheckNames)),
	}
	for _, name := range orderedCheckNames {
		report.Checks = append(report.Checks, checkResult{
			Name:    name,
			Status:  statusNotRun,
			Summary: "The check was not executed",
		})
	}
	return report
}

func (r *e2eReport) setCheck(name string, status checkStatus, summary string) error {
	if !validCheckStatus(status) {
		return fmt.Errorf("unsupported check status %q", status)
	}
	if strings.TrimSpace(summary) == "" || strings.ContainsAny(summary, "\r\n") {
		return fmt.Errorf("check summary must be one non-empty line")
	}
	if !validCheckSummary(summary) {
		return fmt.Errorf("check summary must use a fixed redacted value")
	}
	for index := range r.Checks {
		if r.Checks[index].Name == name {
			r.Checks[index].Status = status
			r.Checks[index].Summary = summary
			return nil
		}
	}
	return fmt.Errorf("unknown report check %q", name)
}

func validCheckSummary(summary string) bool {
	switch summary {
	case checkSummaryPassed, checkSummaryFailed, checkSummaryEvidenceNotConfigured,
		checkSummaryAuditNotConfigured, checkSummaryFaultNotConfigured:
		return true
	default:
		return false
	}
}

func validCheckStatus(status checkStatus) bool {
	switch status {
	case statusPassed, statusFailed, statusSkipped, statusNotRun:
		return true
	default:
		return false
	}
}

func writeE2EArtifacts(directory string, report *e2eReport) error {
	if report == nil {
		return fmt.Errorf("report must not be nil")
	}
	if report.CompletedAt.IsZero() {
		report.CompletedAt = time.Now().UTC()
	}
	report.Status = overallStatus(report.Checks)
	parent := filepath.Dir(filepath.Clean(directory))
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create artifact parent directory: %w", err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return fmt.Errorf("create exclusive artifact directory: %w", err)
	}
	jsonOutput, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode JSON artifact: %w", err)
	}
	jsonOutput = append(jsonOutput, '\n')
	if err := writeExclusiveFile(filepath.Join(directory, "report.json"), jsonOutput); err != nil {
		return fmt.Errorf("write JSON artifact: %w", err)
	}
	if err := writeExclusiveFile(filepath.Join(directory, "report.md"), renderMarkdownReport(report)); err != nil {
		return fmt.Errorf("write Markdown artifact: %w", err)
	}
	return nil
}

func renderMarkdownReport(report *e2eReport) []byte {
	var output bytes.Buffer
	renderMarkdownReportHeader(&output, report)
	renderMarkdownCheckTable(&output, report.Checks)
	renderMarkdownAuditEvidence(&output, report.Audit)
	renderMarkdownMetricsEvidence(&output, report.Metrics)
	renderMarkdownLimits(&output, report.ProjectMode)
	return output.Bytes()
}

func renderMarkdownReportHeader(output *bytes.Buffer, report *e2eReport) {
	_, _ = fmt.Fprintln(output, "# Phase 2 E2E foundations report")
	_, _ = fmt.Fprintln(output)
	_, _ = fmt.Fprintf(output, "Status: %s\n\n", report.Status)
	_, _ = fmt.Fprintf(output, "Started: %s\n\n", report.StartedAt.Format(time.RFC3339))
	_, _ = fmt.Fprintf(output, "Completed: %s\n\n", report.CompletedAt.Format(time.RFC3339))
	_, _ = fmt.Fprintf(output, "Gateway API bundle: %s\n\n", report.GatewayAPIBundle)
	_, _ = fmt.Fprintf(output, "Controller revision: `%s`\n\n", report.ControllerRevision)
	_, _ = fmt.Fprintf(output, "Controller image digest: `%s`\n\n", report.ControllerImageDigest)
	_, _ = fmt.Fprintf(output, "OpenStack project mode: %s\n\n", report.ProjectMode)
	_, _ = fmt.Fprintf(output, "Restart mode: %s\n\n", report.RestartMode)
}

func renderMarkdownCheckTable(output *bytes.Buffer, checks []checkResult) {
	_, _ = fmt.Fprintln(output, "| Check | Result | Notes |")
	_, _ = fmt.Fprintln(output, "| --- | --- | --- |")
	for _, check := range checks {
		_, _ = fmt.Fprintf(
			output,
			"| %s | %s | %s |\n",
			markdownCell(check.Name),
			markdownCell(string(check.Status)),
			markdownCell(check.Summary),
		)
	}
}

func renderMarkdownAuditEvidence(output *bytes.Buffer, evidence *auditEvidence) {
	if evidence == nil {
		return
	}
	_, _ = fmt.Fprintln(output, "\n## Ownership audit counts")
	_, _ = fmt.Fprintln(output)
	_, _ = fmt.Fprintln(output, "Only aggregate counts are retained. Resource and object identifiers are omitted.")
	renderMarkdownAuditSummary(output, "Before the test", evidence.Baseline)
	renderMarkdownAuditSummary(output, "Active inventory", evidence.Active)
	renderMarkdownAuditSummary(output, "After leader recovery", evidence.AfterLeader)
	renderMarkdownAuditSummary(output, "After cold restart", evidence.AfterRestart)
	renderMarkdownAuditSummary(output, "After cleanup", evidence.AfterCleanup)
}

func renderMarkdownAuditSummary(output *bytes.Buffer, label string, summary *auditSummary) {
	if summary == nil {
		return
	}
	_, _ = fmt.Fprintf(
		output,
		"\n- %s: assessment=%s, bindings=%d, resources=%d, matched=%d, issues=%d, orphan candidates=%d, stale UIDs=%d, conflicts=%d, unresolved=%d\n",
		label,
		summary.Assessment,
		summary.Bindings,
		summary.Resources,
		summary.Matched,
		summary.KubernetesIssues,
		summary.OrphanCandidates,
		summary.StaleUIDs,
		summary.Conflicts,
		summary.Unresolved,
	)
}

func renderMarkdownMetricsEvidence(output *bytes.Buffer, evidence *metricsEvidence) {
	if evidence == nil {
		return
	}
	_, _ = fmt.Fprintln(output, "\n## Metrics snapshot")
	_, _ = fmt.Fprintln(output)
	_, _ = fmt.Fprintln(output, "The artifact contains only aggregate values from an allowlist. Labels are omitted.")
}

func renderMarkdownLimits(output *bytes.Buffer, mode projectMode) {
	_, _ = fmt.Fprintln(output, "\n## Limits")
	_, _ = fmt.Fprintln(output)
	_, _ = fmt.Fprintln(output, "This artifact contains no raw audit report, Kubernetes or OpenStack resource identifiers, published address, pod name, credentials, or API response body.")
	if mode == projectModeShared {
		_, _ = fmt.Fprintln(output, "Shared-project mode provides run-scoped checks, not an OpenStack authorization boundary. A Passed result does not prove that unrelated resources were inaccessible or that a project-wide inventory had no run-attributable residue.")
	}
}

func overallStatus(checks []checkResult) checkStatus {
	passedRequired := make(map[string]struct{}, len(requiredBaselineChecks))
	for _, check := range checks {
		if check.Status == statusFailed {
			return statusFailed
		}
		if _, required := requiredBaselineChecks[check.Name]; required && check.Status == statusPassed {
			passedRequired[check.Name] = struct{}{}
		}
	}
	if len(passedRequired) == len(requiredBaselineChecks) {
		return statusPassed
	}
	return statusNotRun
}

func writeExclusiveFile(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	written, writeErr := file.Write(contents)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if written != len(contents) {
		return fmt.Errorf("short artifact write")
	}
	return closeErr
}

func markdownCell(value string) string {
	value = strings.ReplaceAll(value, "|", "\\|")
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	return value
}
