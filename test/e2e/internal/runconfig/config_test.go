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
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func TestLoadRejectsUnknownYAMLFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := []byte("formatVersion: v1alpha1\nunknownField: true\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknownField") {
		t.Fatalf("Load() error = %v, want unknown-field rejection", err)
	}
}

func TestResolveSupportsDedicatedAndSharedProjects(t *testing.T) {
	tests := []struct {
		name string
		mode ProjectMode
		edit func(*File)
	}{
		{
			name: "dedicated",
			mode: ProjectModeDedicated,
			edit: func(config *File) {
				config.OpenStack.ProjectMode = ProjectModeDedicated
				config.OpenStack.DedicatedForE2E = true
				config.OpenStack.AcceptProjectWideCredentialRisk = false
			},
		},
		{
			name: "shared",
			mode: ProjectModeShared,
			edit: func(config *File) {
				config.OpenStack.ProjectMode = ProjectModeShared
				config.OpenStack.DedicatedForE2E = false
				config.OpenStack.AcceptProjectWideCredentialRisk = true
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validFile()
			test.edit(&config)
			runtime, err := Resolve(config, ResolveOptions{RepositoryRoot: t.TempDir()})
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if runtime.Project.Mode != test.mode {
				t.Fatalf("runtime project/audit = %#v, %#v", runtime.Project, runtime.Audit)
			}
			if runtime.Project.ExpectedProjectID != config.OpenStack.ExpectedProjectID ||
				runtime.Project.ExpectedVIPSubnetID != config.OpenStack.VIPSubnetID ||
				runtime.Project.ExpectedMemberSubnetID != config.OpenStack.MemberSubnetID ||
				runtime.Project.ExpectedExternalNetworkID != "" {
				t.Fatalf("runtime project = %#v", runtime.Project)
			}
			if runtime.Audit.CloudsYAML != config.OpenStack.ControllerCloudsYAML {
				t.Fatalf("audit clouds.yaml = %q, want controller credential reuse", runtime.Audit.CloudsYAML)
			}
		})
	}
}

func TestResolveRequiresModeSpecificAcknowledgement(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*File)
		want   string
	}{
		{
			name: "dedicated acknowledgement missing",
			mutate: func(config *File) {
				config.OpenStack.ProjectMode = ProjectModeDedicated
				config.OpenStack.DedicatedForE2E = false
				config.OpenStack.AcceptProjectWideCredentialRisk = false
			},
			want: "dedicatedForE2E must be exactly true",
		},
		{
			name: "shared risk acknowledgement missing",
			mutate: func(config *File) {
				config.OpenStack.ProjectMode = ProjectModeShared
				config.OpenStack.DedicatedForE2E = false
				config.OpenStack.AcceptProjectWideCredentialRisk = false
			},
			want: "acceptProjectWideCredentialRisk must be exactly true",
		},
		{
			name: "shared acknowledgement set in dedicated mode",
			mutate: func(config *File) {
				config.OpenStack.ProjectMode = ProjectModeDedicated
				config.OpenStack.DedicatedForE2E = true
				config.OpenStack.AcceptProjectWideCredentialRisk = true
			},
			want: "acceptProjectWideCredentialRisk must be false",
		},
		{
			name: "dedicated acknowledgement set in shared mode",
			mutate: func(config *File) {
				config.OpenStack.ProjectMode = ProjectModeShared
				config.OpenStack.DedicatedForE2E = true
				config.OpenStack.AcceptProjectWideCredentialRisk = true
			},
			want: "dedicatedForE2E must be false",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validFile()
			test.mutate(&config)
			_, err := Resolve(config, ResolveOptions{RepositoryRoot: t.TempDir()})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Resolve() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestResolveDerivesOneRunScopedIdentity(t *testing.T) {
	config := validFile()
	config.RunID = ""
	runtime, err := Resolve(config, ResolveOptions{
		RepositoryRoot: t.TempDir(),
		Now:            func() time.Time { return time.Date(2026, 9, 1, 12, 34, 56, 0, time.UTC) },
		Random:         bytes.NewReader([]byte{0xaa, 0xbb, 0xcc, 0xdd}),
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if runtime.RunID != "run-20260901-123456-aabbccdd" ||
		runtime.Namespace != "gateway-api-openstack-e2e-"+runtime.RunID ||
		runtime.ControllerNamespace != "openstack-gateway-e2e-"+runtime.RunID ||
		runtime.GatewayClassName != "gao-e2e-"+runtime.RunID ||
		runtime.Audit.ClusterID != runtime.GatewayClassName ||
		runtime.ControllerName != "e2e.example.test/"+runtime.GatewayClassName {
		t.Fatalf("derived runtime identity = %#v", runtime)
	}
}

func TestResolveRejectsMissingCommonSafetyInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*File)
		want   string
	}{
		{name: "Kubernetes acknowledgement", mutate: func(config *File) { config.Kubernetes.DedicatedForE2E = false }, want: "kubernetes.dedicatedForE2E"},
		{name: "project ID", mutate: func(config *File) { config.OpenStack.ExpectedProjectID = "" }, want: "expectedProjectID"},
		{name: "VIP subnet", mutate: func(config *File) { config.OpenStack.VIPSubnetID = "" }, want: "vipSubnetID"},
		{name: "member subnet", mutate: func(config *File) { config.OpenStack.MemberSubnetID = "" }, want: "memberSubnetID"},
		{name: "external network choice", mutate: func(config *File) { config.OpenStack.ExternalNetworkID = "" }, want: "externalNetworkID"},
		{name: "controller credential", mutate: func(config *File) { config.OpenStack.ControllerCloudsYAML = "" }, want: "controllerCloudsYAML"},
		{name: "controller image", mutate: func(config *File) { config.Controller.Image = "registry.example.test/controller:latest" }, want: "controller.image"},
		{name: "backend image", mutate: func(config *File) { config.Backend.Image = "registry.example.test/agnhost:latest" }, want: "backend.image"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validFile()
			test.mutate(&config)
			_, err := Resolve(config, ResolveOptions{RepositoryRoot: t.TempDir()})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Resolve() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestResolveBackendProfiles(t *testing.T) {
	tests := []struct {
		name       string
		policy     corev1.ServiceExternalTrafficPolicy
		selector   map[string]string
		wantPolicy corev1.ServiceExternalTrafficPolicy
		wantError  string
	}{
		{name: "default Cluster", wantPolicy: corev1.ServiceExternalTrafficPolicyCluster},
		{name: "explicit Cluster", policy: corev1.ServiceExternalTrafficPolicyCluster, wantPolicy: corev1.ServiceExternalTrafficPolicyCluster},
		{name: "Local", policy: corev1.ServiceExternalTrafficPolicyLocal, wantPolicy: corev1.ServiceExternalTrafficPolicyLocal},
		{name: "selected Local", policy: corev1.ServiceExternalTrafficPolicyLocal, selector: map[string]string{"e2e.example.test/backend": "true"}, wantPolicy: corev1.ServiceExternalTrafficPolicyLocal},
		{name: "empty label value", policy: corev1.ServiceExternalTrafficPolicyLocal, selector: map[string]string{"node-role.kubernetes.io/worker": ""}, wantPolicy: corev1.ServiceExternalTrafficPolicyLocal},
		{name: "unknown policy", policy: "local", wantError: "externalTrafficPolicy"},
		{name: "selector with default Cluster", selector: map[string]string{"backend": "true"}, wantError: "requires"},
		{name: "selector with explicit Cluster", policy: corev1.ServiceExternalTrafficPolicyCluster, selector: map[string]string{"backend": "true"}, wantError: "requires"},
		{name: "invalid selector key", policy: corev1.ServiceExternalTrafficPolicyLocal, selector: map[string]string{"invalid/private/key": "true"}, wantError: "valid Kubernetes labels"},
		{name: "invalid selector value", policy: corev1.ServiceExternalTrafficPolicyLocal, selector: map[string]string{"backend": "private value"}, wantError: "valid Kubernetes labels"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validFile()
			config.Backend.ExternalTrafficPolicy = test.policy
			config.Backend.NodeSelector = test.selector
			runtime, err := Resolve(config, ResolveOptions{RepositoryRoot: t.TempDir()})
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) || strings.Contains(err.Error(), "private") {
					t.Fatalf("Resolve() error = %v, want redacted %q rejection", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if runtime.BackendExternalTrafficPolicy != test.wantPolicy || !reflect.DeepEqual(runtime.BackendNodeSelector, test.selector) {
				t.Fatal("Resolve() did not preserve the backend profile")
			}
			for key := range config.Backend.NodeSelector {
				config.Backend.NodeSelector[key] = "changed"
				if runtime.BackendNodeSelector[key] == "changed" {
					t.Fatal("runtime selector aliases the input configuration")
				}
			}
		})
	}
}

func validFile() File {
	digest := "sha256:" + strings.Repeat("a", 64)
	return File{
		FormatVersion: FormatVersion,
		RunID:         "run-20260901",
		Kubernetes: Kubernetes{
			DedicatedForE2E: true,
			Kubeconfig:      "/tmp/e2e.kubeconfig",
			Context:         "openstack-e2e",
		},
		OpenStack: OpenStack{
			ProjectMode:                     ProjectModeShared,
			AcceptProjectWideCredentialRisk: true,
			ExpectedProjectID:               "project-id",
			Cloud:                           "openstack-e2e",
			Region:                          "RegionOne",
			ControllerCloudsYAML:            "/tmp/controller-clouds.yaml",
			VIPSubnetID:                     "vip-subnet-id",
			MemberSubnetID:                  "member-subnet-id",
			ExternalNetworkID:               disabledNetwork,
		},
		Controller: Controller{
			Domain:         "e2e.example.test",
			Image:          "registry.example.test/controller@" + digest,
			SourceRevision: strings.Repeat("b", 40),
		},
		Backend:   Backend{Image: "registry.example.test/agnhost:1.0@" + digest},
		Artifacts: Artifacts{Root: "/tmp/e2e-artifacts"},
	}
}
