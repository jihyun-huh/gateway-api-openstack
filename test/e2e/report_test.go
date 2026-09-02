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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOverallStatusRequiresBaseline(t *testing.T) {
	report := newE2EReport(time.Unix(1, 0), projectModeDedicated, "cold", strings.Repeat("b", 40), "sha256:"+strings.Repeat("a", 64))
	if got := overallStatus(report.Checks); got != statusNotRun {
		t.Fatalf("overallStatus() = %q, want %q", got, statusNotRun)
	}
	for name := range requiredBaselineChecks {
		if err := report.setCheck(name, statusPassed, checkSummaryPassed); err != nil {
			t.Fatal(err)
		}
	}
	if got := overallStatus(report.Checks); got != statusPassed {
		t.Fatalf("overallStatus() = %q, want %q", got, statusPassed)
	}
	if err := report.setCheck("quota failure", statusFailed, checkSummaryFailed); err != nil {
		t.Fatal(err)
	}
	if got := overallStatus(report.Checks); got != statusFailed {
		t.Fatalf("overallStatus() = %q, want %q", got, statusFailed)
	}
}

func TestSetCheckRejectsUnsafeSummaryShape(t *testing.T) {
	report := newE2EReport(time.Now(), projectModeDedicated, "cold", strings.Repeat("b", 40), "sha256:"+strings.Repeat("a", 64))
	if err := report.setCheck("Gateway status", statusPassed, "line one\nline two"); err == nil {
		t.Fatal("setCheck() accepted a multiline summary")
	}
	if err := report.setCheck("missing check", statusPassed, "Safe summary"); err == nil {
		t.Fatal("setCheck() accepted an unknown check")
	}
	if err := report.setCheck("Gateway status", statusFailed, "secret-token"); err == nil {
		t.Fatal("setCheck() accepted a non-fixed summary")
	}
}

func TestWriteE2EArtifactsIsExclusiveAndSanitized(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "run-1234")
	revision := strings.Repeat("b", 40)
	digest := "sha256:" + strings.Repeat("a", 64)
	report := newE2EReport(time.Unix(1, 0), projectModeShared, "cold", revision, digest)
	report.CompletedAt = time.Unix(2, 0)
	if err := report.setCheck("preflight safety validation", statusPassed, checkSummaryPassed); err != nil {
		t.Fatal(err)
	}
	if err := writeE2EArtifacts(directory, report); err != nil {
		t.Fatalf("writeE2EArtifacts() error = %v", err)
	}
	for _, name := range []string{"report.json", "report.md"} {
		assertE2EArtifactContents(t, filepath.Join(directory, name), revision, digest)
	}
	if err := writeE2EArtifacts(directory, report); err == nil {
		t.Fatal("writeE2EArtifacts() overwrote an existing artifact directory")
	}
}

func assertE2EArtifactContents(t *testing.T, path, revision, digest string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(contents)
	for _, forbidden := range []string{"secret-token", "project-id", "expected-vip-subnet", "expected-member-subnet", "192.0.2.10", "pod-uid"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("%s contains forbidden value %q", filepath.Base(path), forbidden)
		}
	}
	if !strings.Contains(text, revision) || !strings.Contains(text, digest) || !strings.Contains(text, string(projectModeShared)) {
		t.Fatalf("%s does not contain immutable controller evidence", filepath.Base(path))
	}
}
