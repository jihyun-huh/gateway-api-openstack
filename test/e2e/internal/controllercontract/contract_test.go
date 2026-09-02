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

package controllercontract

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestSettingsEnvironmentReturnsCompleteIndependentMap(t *testing.T) {
	settings := Settings{
		ControllerName:    "e2e.example.test/gao-e2e-run-1234",
		ClusterID:         "gao-e2e-run-1234",
		VIPSubnetID:       "vip-subnet",
		ExternalNetworkID: "",
		MemberSubnetID:    "member-subnet",
		Cloud:             "openstack-e2e",
		Region:            "RegionOne",
	}
	want := map[string]string{
		EnvControllerName:    settings.ControllerName,
		EnvClusterID:         settings.ClusterID,
		EnvOctaviaProvider:   "amphora",
		EnvVIPSubnetID:       settings.VIPSubnetID,
		EnvExternalNetworkID: "",
		EnvMemberSubnetID:    settings.MemberSubnetID,
		EnvMemberMode:        "NodePort",
		EnvNodeAddressType:   "InternalIP",
		EnvHealthPath:        "/",
		EnvResyncInterval:    "10m",
		EnvAPIQPS:            "10",
		EnvAPIBurst:          "20",
		EnvCloudsYAML:        "/etc/openstack/clouds.yaml",
		EnvCloud:             settings.Cloud,
		EnvRegion:            settings.Region,
	}
	got := settings.Environment()
	if len(got) != len(want) {
		t.Fatalf("Environment() has %d entries, want %d", len(got), len(want))
	}
	for name, value := range want {
		if got[name] != value || !IsProtectedEnvironment(name) {
			t.Errorf("environment[%s] = %q, protected = %t, want %q, true", name, got[name], IsProtectedEnvironment(name), value)
		}
	}
	got[EnvControllerName] = "changed"
	if settings.Environment()[EnvControllerName] != settings.ControllerName {
		t.Fatal("Environment() returned shared mutable state")
	}
	if IsProtectedEnvironment("UNRELATED") {
		t.Fatal("unrelated environment variable is protected")
	}
}

func TestValidateArguments(t *testing.T) {
	valid := []string{
		"--leader-elect=true",
		"--metrics-bind-address=:8080",
		"--octavia-microversion=2.5",
		"--zap-log-level=2",
	}
	if err := ValidateArguments(valid, "2.5"); err != nil {
		t.Fatalf("ValidateArguments() error = %v", err)
	}
	tests := []struct {
		name      string
		arguments []string
		version   string
	}{
		{name: "empty microversion", arguments: valid, version: ""},
		{name: "protected override", arguments: append(append([]string(nil), valid...), "--controller-name=other.example.test/controller"), version: "2.5"},
		{name: "missing required", arguments: valid[1:], version: "2.5"},
		{name: "duplicate required", arguments: append(append([]string(nil), valid...), "--leader-elect=true"), version: "2.5"},
		{name: "wrong required value", arguments: []string{"--leader-elect=false", valid[1], valid[2]}, version: "2.5"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateArguments(test.arguments, test.version); err == nil {
				t.Fatal("ValidateArguments() accepted an unsafe argument contract")
			}
		})
	}
	for _, name := range []string{"controller-name", "--cluster-id", "vip-subnet-id"} {
		if !IsProtectedArgument(name) {
			t.Errorf("IsProtectedArgument(%q) = false", name)
		}
	}
	if IsProtectedArgument("zap-log-level") {
		t.Fatal("unrelated argument is protected")
	}
}

func TestValidateMetricsPorts(t *testing.T) {
	valid := []corev1.ContainerPort{{Name: MetricsPortName, ContainerPort: MetricsPort, Protocol: corev1.ProtocolTCP}}
	if err := ValidateMetricsPorts(valid); err != nil {
		t.Fatalf("ValidateMetricsPorts() error = %v", err)
	}
	for _, test := range []struct {
		name  string
		ports []corev1.ContainerPort
	}{
		{name: "absent"},
		{name: "wrong number", ports: []corev1.ContainerPort{{Name: MetricsPortName, ContainerPort: 9090, Protocol: corev1.ProtocolTCP}}},
		{name: "wrong protocol", ports: []corev1.ContainerPort{{Name: MetricsPortName, ContainerPort: MetricsPort, Protocol: corev1.ProtocolUDP}}},
		{name: "duplicate", ports: append(append([]corev1.ContainerPort(nil), valid...), valid...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateMetricsPorts(test.ports); err == nil {
				t.Fatal("ValidateMetricsPorts() accepted an unsafe port contract")
			}
		})
	}
}

func TestSourceVersionRequiresExactAPIIdentity(t *testing.T) {
	got, err := SourceVersion(types.UID("source-uid"), "17")
	if err != nil || got != "source-uid/17" {
		t.Fatalf("SourceVersion() = %q, %v", got, err)
	}
	for _, test := range []struct {
		uid     types.UID
		version string
	}{
		{version: "17"},
		{uid: types.UID("source-uid")},
		{uid: types.UID(" source-uid"), version: "17"},
		{uid: types.UID("source-uid"), version: "17 "},
	} {
		value, err := SourceVersion(test.uid, test.version)
		if err == nil || value != "" {
			t.Fatalf("SourceVersion(%q, %q) = %q, %v", test.uid, test.version, value, err)
		}
		if test.uid != "" && strings.Contains(err.Error(), string(test.uid)) {
			t.Fatalf("SourceVersion() leaked UID %q: %v", test.uid, err)
		}
	}
}
