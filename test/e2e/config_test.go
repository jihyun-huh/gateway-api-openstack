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
	"path/filepath"
	"strings"
	"testing"

	"github.com/jihyun-huh/gateway-api-openstack/test/e2e/internal/runconfig"
)

func TestLoadE2EConfigRequiresExactOptIn(t *testing.T) {
	for _, value := range []string{"", "false", "TRUE", " true"} {
		t.Run(value, func(t *testing.T) {
			config, enabled, err := loadE2EConfig(mapEnvironment(map[string]string{
				enableEnvironment: value,
			}))
			if err != nil || enabled || config != (e2eConfig{}) {
				t.Fatalf("loadE2EConfig() = %#v, %t, %v", config, enabled, err)
			}
		})
	}
}

func TestLoadE2EConfigRequiresAbsoluteRuntimePath(t *testing.T) {
	for _, path := range []string{"", "runtime.json"} {
		_, enabled, err := loadE2EConfig(mapEnvironment(map[string]string{
			enableEnvironment:        "true",
			runtimeConfigEnvironment: path,
		}))
		if !enabled || err == nil || !strings.Contains(err.Error(), runtimeConfigEnvironment) {
			t.Fatalf("loadE2EConfig(%q) enabled = %t, error = %v", path, enabled, err)
		}
	}
}

func TestLoadE2EConfigUsesValidatedPrivateRuntime(t *testing.T) {
	for _, mode := range []runconfig.ProjectMode{runconfig.ProjectModeDedicated, runconfig.ProjectModeShared} {
		t.Run(string(mode), func(t *testing.T) {
			runtime := validRuntimeConfig(t, mode)
			path, err := runconfig.WriteRuntime(t.TempDir(), runtime)
			if err != nil {
				t.Fatal(err)
			}
			config, enabled, err := loadE2EConfig(mapEnvironment(map[string]string{
				enableEnvironment:        "true",
				runtimeConfigEnvironment: path,
			}))
			if err != nil || !enabled {
				t.Fatalf("loadE2EConfig() enabled = %t, error = %v", enabled, err)
			}
			if config.Project.Mode != mode || config.RunID != runtime.RunID || config.ControllerName != runtime.ControllerName {
				t.Fatalf("loaded runtime = %#v", config)
			}
		})
	}
}

func validRuntimeConfig(t *testing.T, mode runconfig.ProjectMode) runconfig.Runtime {
	t.Helper()
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	file := runconfig.File{
		FormatVersion: runconfig.FormatVersion,
		RunID:         "run-20260902",
		Kubernetes: runconfig.Kubernetes{
			DedicatedForE2E: true,
			Kubeconfig:      "/tmp/e2e.kubeconfig",
			Context:         "openstack-e2e",
		},
		OpenStack: runconfig.OpenStack{
			ProjectMode:                     mode,
			DedicatedForE2E:                 mode == runconfig.ProjectModeDedicated,
			AcceptProjectWideCredentialRisk: mode == runconfig.ProjectModeShared,
			ExpectedProjectID:               "project-id",
			Cloud:                           "openstack-e2e",
			Region:                          "RegionOne",
			ControllerCloudsYAML:            "/tmp/controller-clouds.yaml",
			AuditCloudsYAML:                 "/tmp/audit-clouds.yaml",
			VIPSubnetID:                     "vip-subnet-id",
			MemberSubnetID:                  "member-subnet-id",
			ExternalNetworkID:               "none",
		},
		Controller: runconfig.Controller{
			Domain:         "e2e.example.test",
			Image:          "registry.example.test/controller@" + digest,
			SourceRevision: strings.Repeat("b", 40),
		},
		Backend: runconfig.Backend{Image: "registry.example.test/agnhost:1.0@" + digest},
	}
	runtime, err := runconfig.Resolve(file, runconfig.ResolveOptions{RepositoryRoot: repositoryRoot})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func mapEnvironment(values map[string]string) environmentReader {
	return func(name string) string { return values[name] }
}
