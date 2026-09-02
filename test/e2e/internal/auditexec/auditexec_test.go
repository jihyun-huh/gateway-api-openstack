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

package auditexec

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/jihyun-huh/gateway-api-openstack/internal/audit"
)

func TestRunBuildsArgumentsAndValidatesEmptyReport(t *testing.T) {
	config := validConfig()
	report := validReport()
	config.Environment = []string{"PATH=/bin", "SAFE=value"}
	config.RunCommand = func(_ context.Context, binary string, arguments, environment []string, stdout, _ io.Writer) error {
		if binary != config.Binary {
			t.Fatalf("binary = %q, want %q", binary, config.Binary)
		}
		wantArguments := []string{
			"--controller-name=" + config.ControllerName,
			"--cluster-id=" + config.ClusterID,
			"--kubeconfig=" + config.Kubeconfig,
			"--context=" + config.Context,
			"--octavia-microversion=" + config.Microversion,
			"--clouds-yaml=" + config.CloudsYAML,
			"--openstack-cloud=" + config.Cloud,
			"--openstack-region=" + config.Region,
		}
		if !slices.Equal(arguments, wantArguments) || !slices.Equal(environment, config.Environment) {
			t.Fatalf("arguments = %#v, environment = %#v", arguments, environment)
		}
		contents, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		_, err = stdout.Write(contents)
		return err
	}
	got, err := Run(context.Background(), config, Validation{RequireEmpty: true})
	if err != nil || got.Scope != report.Scope {
		t.Fatalf("Run() = %#v, %v", got, err)
	}
}

func TestRunBoundsOutputAndRedactsCommandErrors(t *testing.T) {
	config := validConfig()
	config.OutputLimit = 4
	config.RunCommand = func(_ context.Context, _ string, _ []string, _ []string, stdout, stderr io.Writer) error {
		_, _ = stdout.Write([]byte("12345"))
		_, _ = stderr.Write([]byte("credential-secret-sentinel"))
		return errors.New("credential-secret-sentinel")
	}
	_, err := Run(context.Background(), config, Validation{})
	if err == nil || strings.Contains(err.Error(), "credential-secret-sentinel") || !strings.Contains(err.Error(), "safe in-memory limit") {
		t.Fatalf("Run() error = %v", err)
	}

	config.OutputLimit = defaultOutputLimit
	config.RunCommand = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
		return errors.New("credential-secret-sentinel")
	}
	_, err = Run(context.Background(), config, Validation{})
	if err == nil || strings.Contains(err.Error(), "credential-secret-sentinel") {
		t.Fatalf("Run() command error = %v", err)
	}
}

func TestRunRejectsInvalidJSON(t *testing.T) {
	config := validConfig()
	config.RunCommand = func(_ context.Context, _ string, _ []string, _ []string, stdout, _ io.Writer) error {
		_, err := stdout.Write([]byte("not-json"))
		return err
	}
	if _, err := Run(context.Background(), config, Validation{}); err == nil {
		t.Fatal("Run() accepted invalid JSON")
	}
}

func TestValidateReport(t *testing.T) {
	matched := audit.ResourceFinding{Disposition: audit.DispositionMatched}
	tests := []struct {
		name         string
		mutate       func(*audit.Report)
		requireEmpty bool
		wantErr      bool
	}{
		{name: "empty", requireEmpty: true},
		{name: "matched active resource", mutate: func(report *audit.Report) {
			report.Resources = []audit.ResourceFinding{matched}
			report.Summary.OpenStackResources = 1
			report.Summary.Matched = 1
		}},
		{name: "matched resource must be absent from empty scope", mutate: func(report *audit.Report) {
			report.Resources = []audit.ResourceFinding{matched}
			report.Summary.OpenStackResources = 1
			report.Summary.Matched = 1
		}, requireEmpty: true, wantErr: true},
		{name: "unsupported format", mutate: func(report *audit.Report) { report.FormatVersion = "future" }, wantErr: true},
		{name: "incomplete", mutate: func(report *audit.Report) { report.Assessment = audit.AssessmentIncomplete }, wantErr: true},
		{name: "scope mismatch", mutate: func(report *audit.Report) { report.Scope.ClusterID = "foreign-project-sentinel" }, wantErr: true},
		{name: "summary finding", mutate: func(report *audit.Report) { report.Summary.Unresolved = 1 }, wantErr: true},
		{name: "issue list with inconsistent summary", mutate: func(report *audit.Report) {
			report.KubernetesIssues = []audit.KubernetesIssue{{Reason: "invalid"}}
		}, wantErr: true},
		{name: "unmatched resource with inconsistent summary", mutate: func(report *audit.Report) {
			report.Resources = []audit.ResourceFinding{{Disposition: audit.DispositionUnresolved}}
		}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := validReport()
			if test.mutate != nil {
				test.mutate(&report)
			}
			err := ValidateReport(report, "e2e.example.test/controller", "cluster-a", Validation{RequireEmpty: test.requireEmpty})
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateReport() error = %v, wantErr %t", err, test.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "foreign-project-sentinel") {
				t.Fatalf("ValidateReport() leaked scope value: %v", err)
			}
		})
	}
}

func TestArgumentsRejectsIncompleteConfig(t *testing.T) {
	config := validConfig()
	config.Binary = ""
	if _, err := Arguments(config); err == nil {
		t.Fatal("Arguments() accepted an empty binary")
	}
	config = validConfig()
	config.Cloud = ""
	if _, err := Arguments(config); err == nil {
		t.Fatal("Arguments() accepted clouds.yaml without a cloud")
	}
}

func TestBoundedBufferRetainsAtMostLimit(t *testing.T) {
	buffer := boundedBuffer{limit: 4}
	contents := []byte("sensitive-output")
	written, err := buffer.Write(contents)
	if err != nil || written != len(contents) || !buffer.overflow || len(buffer.Bytes()) != 4 {
		t.Fatalf("boundedBuffer.Write() = %d, %v; overflow = %t, retained = %d", written, err, buffer.overflow, len(buffer.Bytes()))
	}
}

func validConfig() Config {
	return Config{
		Binary:         "/workspace/bin/openstack-gateway-audit",
		ControllerName: "e2e.example.test/controller",
		ClusterID:      "cluster-a",
		Kubeconfig:     "/workspace/kubeconfig",
		Context:        "e2e",
		Microversion:   "2.5",
		CloudsYAML:     "/workspace/clouds.yaml",
		Cloud:          "openstack-e2e",
		Region:         "RegionOne",
	}
}

func validReport() audit.Report {
	return audit.Report{
		FormatVersion: audit.ReportFormatVersion,
		Assessment:    audit.AssessmentComplete,
		Scope: audit.ReportScope{
			ControllerName: "e2e.example.test/controller",
			ClusterID:      "cluster-a",
		},
	}
}
