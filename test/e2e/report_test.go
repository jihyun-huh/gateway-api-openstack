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

	corev1 "k8s.io/api/core/v1"

	"github.com/jihyun-huh/gateway-api-openstack/test/e2e/internal/runconfig"
)

func TestOverallStatusRequiresBaseline(t *testing.T) {
	report := newE2EReport(time.Unix(1, 0), reportTestConfig(projectModeDedicated))
	if got := overallStatus(report.Checks); got != statusNotRun {
		t.Fatalf("overallStatus() = %q, want %q", got, statusNotRun)
	}
	for _, check := range reportChecks {
		if !check.required {
			continue
		}
		if err := report.setCheck(check.name, statusPassed, checkSummaryPassed); err != nil {
			t.Fatal(err)
		}
	}
	if got := overallStatus(report.Checks); got != statusPassed {
		t.Fatalf("overallStatus() = %q, want %q", got, statusPassed)
	}
	for _, test := range []struct {
		name   string
		status checkStatus
		want   checkStatus
	}{
		{name: "orderly deletion and finalizer completion", status: statusNotRun, want: statusNotRun},
		{name: "orderly deletion and finalizer completion", status: statusSkipped, want: statusNotRun},
		{name: "orderly deletion and finalizer completion", status: statusFailed, want: statusFailed},
		{name: "quota failure", status: statusFailed, want: statusFailed},
	} {
		t.Run(test.name+"/"+string(test.status), func(t *testing.T) {
			checks := append([]checkResult(nil), report.Checks...)
			for i := range checks {
				if checks[i].Name == test.name {
					checks[i].Status = test.status
				}
			}
			if got := overallStatus(checks); got != test.want {
				t.Fatalf("overallStatus() = %q, want %q", got, test.want)
			}
		})
	}
	// A duplicate success cannot replace the required cleanup audit.
	checks := append([]checkResult(nil), report.Checks...)
	for i := range checks {
		if checks[i].Name == "post-test ownership audit returns to baseline" {
			checks[i] = checks[0]
		}
	}
	if got := overallStatus(checks); got != statusNotRun {
		t.Fatalf("overallStatus() without cleanup audit = %q, want %q", got, statusNotRun)
	}
}

func TestSetCheckRejectsUnsafeSummaryShape(t *testing.T) {
	report := newE2EReport(time.Now(), reportTestConfig(projectModeDedicated))
	if err := report.setCheck("Gateway status", statusPassed, "line one\nline two"); err == nil {
		t.Fatal("setCheck() accepted a multiline summary")
	}
	if err := report.setCheck("missing check", statusPassed, checkSummaryPassed); err == nil || !strings.Contains(err.Error(), "unknown report check") {
		t.Fatal("setCheck() accepted an unknown check")
	}
	if err := report.setCheck("Gateway status", statusFailed, "secret-token"); err == nil {
		t.Fatal("setCheck() accepted a non-fixed summary")
	}
}

func TestWriteE2EArtifactsIsExclusiveAndSanitized(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "run-1234")
	config := reportTestConfig(projectModeShared)
	config.BackendExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
	config.BackendNodeSelector = map[string]string{"private.example.test/node-pool": "private-node-pool"}
	report := newE2EReport(time.Unix(1, 0), config)
	if report.Backend.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal || !report.Backend.NodeSelectorConfigured {
		t.Fatalf("report backend profile = %#v", report.Backend)
	}
	report.CompletedAt = time.Unix(2, 0)
	if err := report.setCheck("preflight safety validation", statusPassed, checkSummaryPassed); err != nil {
		t.Fatal(err)
	}
	if err := writeE2EArtifacts(directory, report); err != nil {
		t.Fatalf("writeE2EArtifacts() error = %v", err)
	}
	for _, name := range []string{"report.json", "report.md"} {
		assertE2EArtifactContents(t, filepath.Join(directory, name), config.ControllerRevision, config.ControllerImageDigest)
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
	for _, forbidden := range []string{"secret-token", "project-id", "expected-vip-subnet", "expected-member-subnet", "192.0.2.10", "pod-uid", "private.example.test/node-pool", "private-node-pool"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("%s contains forbidden value %q", filepath.Base(path), forbidden)
		}
	}
	if !strings.Contains(text, revision) || !strings.Contains(text, digest) || !strings.Contains(text, string(projectModeShared)) {
		t.Fatalf("%s does not contain immutable controller evidence", filepath.Base(path))
	}
	if !strings.Contains(text, string(corev1.ServiceExternalTrafficPolicyLocal)) {
		t.Fatalf("%s does not record the Local backend profile", filepath.Base(path))
	}
}

func reportTestConfig(mode projectMode) e2eConfig {
	return e2eConfig{
		Project:                      runconfig.Project{Mode: mode},
		RestartMode:                  "cold",
		ControllerRevision:           strings.Repeat("b", 40),
		ControllerImageDigest:        "sha256:" + strings.Repeat("a", 64),
		BackendExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyCluster,
	}
}
