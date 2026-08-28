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
	"strings"
	"testing"
)

func TestLoadE2EConfigRequiresExactOptIn(t *testing.T) {
	for _, value := range []string{"", "false", "TRUE", "1", " true", "true "} {
		t.Run(value, func(t *testing.T) {
			config, enabled, err := loadE2EConfig(func(name string) string {
				if name == enableEnvironment {
					return value
				}
				return ""
			})
			if err != nil || enabled || config != (e2eConfig{}) {
				t.Fatalf("loadE2EConfig() = (%#v, %t, %v), want disabled", config, enabled, err)
			}
		})
	}
}

func TestLoadE2EConfigValidatesSafetyContract(t *testing.T) {
	environment := validEnvironment()
	config, enabled, err := loadE2EConfig(mapEnvironment(environment))
	if err != nil {
		t.Fatalf("loadE2EConfig() error = %v", err)
	}
	if !enabled {
		t.Fatal("loadE2EConfig() did not enable the suite")
	}
	if config.namespace != "gateway-api-openstack-e2e-run-1234" || config.controllerReplicas != 2 {
		t.Fatalf("loadE2EConfig() = %#v", config)
	}
	if config.project.mode != projectModeDedicated {
		t.Fatalf("project mode = %q, want %q", config.project.mode, projectModeDedicated)
	}
	if config.audit.enabled {
		t.Fatal("audit unexpectedly enabled")
	}
}

func TestLoadE2EConfigAcceptsTaggedAgnhostImage(t *testing.T) {
	environment := validEnvironment()
	environment[backendImageEnvironment] = "registry.example.test/e2e/agnhost:2.53@sha256:" + strings.Repeat("c", 64)
	if _, enabled, err := loadE2EConfig(mapEnvironment(environment)); err != nil || !enabled {
		t.Fatalf("loadE2EConfig() enabled = %t, error = %v", enabled, err)
	}
}

func TestLoadE2EConfigRejectsUnsafeValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{name: "invalid project mode", key: projectModeEnvironment, value: "SHARED", want: "dedicated or shared"},
		{name: "project mode has whitespace", key: projectModeEnvironment, value: " shared", want: "dedicated or shared"},
		{name: "dedicated project acknowledgement has whitespace", key: dedicatedProjectEnvironment, value: " true", want: "exactly true"},
		{name: "namespace does not include run ID", key: namespaceEnvironment, value: "default", want: namespaceEnvironment},
		{name: "ambient kubeconfig", key: kubeconfigEnvironment, value: "config", want: kubeconfigEnvironment},
		{name: "single replica", key: controllerReplicasEnvironment, value: "1", want: "at least 2"},
		{name: "mutable controller image", key: controllerImageDigestEnvironment, value: "latest", want: "sha256"},
		{name: "short controller revision", key: controllerRevisionEnvironment, value: "deadbeef", want: "40-character"},
		{name: "invalid container name", key: controllerContainerEnvironment, value: "controller.name", want: "DNS label"},
		{name: "mutable backend image", key: backendImageEnvironment, value: "example/backend:latest", want: "pin"},
		{name: "non-agnhost backend image", key: backendImageEnvironment, value: "registry.example.test/nginx@sha256:" + strings.Repeat("c", 64), want: "agnhost"},
		{name: "root artifact directory", key: artifactDirectoryEnvironment, value: "/", want: "non-root"},
		{name: "artifact path lacks run ID", key: artifactDirectoryEnvironment, value: "/workspace/artifacts", want: "run ID"},
		{name: "unsupported restart", key: restartModeEnvironment, value: "rolling", want: "cold"},
		{name: "suite timeout leaves no cleanup margin", key: timeoutEnvironment, value: "51m", want: "must not exceed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := validEnvironment()
			environment[test.key] = test.value
			_, enabled, err := loadE2EConfig(mapEnvironment(environment))
			if !enabled || err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("loadE2EConfig() enabled = %t, error = %v, want %q", enabled, err, test.want)
			}
		})
	}
}

func TestLoadE2EConfigAcceptsLegacyDedicatedAcknowledgement(t *testing.T) {
	environment := validEnvironment()
	delete(environment, projectModeEnvironment)
	config, enabled, err := loadE2EConfig(mapEnvironment(environment))
	if err != nil || !enabled {
		t.Fatalf("loadE2EConfig() enabled = %t, error = %v", enabled, err)
	}
	if config.project.mode != projectModeDedicated {
		t.Fatalf("project mode = %q, want %q", config.project.mode, projectModeDedicated)
	}
}

func TestLoadE2EConfigAcceptsSharedProjectSafetyContract(t *testing.T) {
	environment := validSharedEnvironment()
	config, enabled, err := loadE2EConfig(mapEnvironment(environment))
	if err != nil || !enabled {
		t.Fatalf("loadE2EConfig() enabled = %t, error = %v", enabled, err)
	}
	if config.project.mode != projectModeShared || config.project.expectedExternalNetworkID != "" || !config.audit.enabled {
		t.Fatalf("shared project config = %#v, audit = %#v", config.project, config.audit)
	}
}

func TestLoadE2EConfigRejectsIncompleteSharedProjectSafetyContract(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{name: "dedicated acknowledgement", key: dedicatedProjectEnvironment, value: "true", want: "unset or exactly false"},
		{name: "audit disabled", key: auditEnableEnvironment, value: "false", want: auditEnableEnvironment},
		{name: "project ID absent", key: expectedProjectIDEnvironment, value: "", want: expectedProjectIDEnvironment},
		{name: "VIP subnet absent", key: expectedVIPSubnetIDEnvironment, value: "", want: expectedVIPSubnetIDEnvironment},
		{name: "member subnet absent", key: expectedMemberSubnetIDEnvironment, value: "", want: expectedMemberSubnetIDEnvironment},
		{name: "external network acknowledgement absent", key: expectedExternalNetworkIDEnvironment, value: "", want: expectedExternalNetworkIDEnvironment},
		{name: "controller Secret absent", key: controllerCloudsSecretEnvironment, value: "", want: controllerCloudsSecretEnvironment},
		{name: "audit clouds file absent", key: auditCloudsYAMLEnvironment, value: "", want: "clouds.yaml"},
		{name: "audit cloud absent", key: auditCloudEnvironment, value: "", want: auditCloudEnvironment},
		{name: "audit region absent", key: auditRegionEnvironment, value: "", want: "region"},
		{name: "cluster identity differs", key: clusterIDEnvironment, value: "another-cluster", want: clusterIDEnvironment},
		{name: "controller name differs", key: controllerNameEnvironment, value: "example.test/another-controller", want: controllerNameEnvironment},
		{name: "controller namespace differs", key: controllerNamespaceEnvironment, value: "gateway-system", want: controllerNamespaceEnvironment},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := validSharedEnvironment()
			environment[test.key] = test.value
			_, enabled, err := loadE2EConfig(mapEnvironment(environment))
			if !enabled || err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("loadE2EConfig() enabled = %t, error = %v, want %q", enabled, err, test.want)
			}
		})
	}
}

func TestLoadE2EConfigAuditIsExplicit(t *testing.T) {
	environment := validEnvironment()
	environment[auditEnableEnvironment] = "true"
	environment[auditBinaryEnvironment] = "/workspace/bin/openstack-gateway-audit"
	environment[clusterIDEnvironment] = "cluster-a"
	environment[auditCloudsYAMLEnvironment] = "/workspace/clouds.yaml"
	environment[auditCloudEnvironment] = "e2e"
	config, enabled, err := loadE2EConfig(mapEnvironment(environment))
	if err != nil || !enabled {
		t.Fatalf("loadE2EConfig() enabled = %t, error = %v", enabled, err)
	}
	if !config.audit.enabled || config.audit.microversion != defaultAuditMicroversion {
		t.Fatalf("audit config = %#v", config.audit)
	}
}

func validEnvironment() map[string]string {
	digest := "sha256:" + strings.Repeat("a", 64)
	return map[string]string{
		enableEnvironment:                "true",
		projectModeEnvironment:           string(projectModeDedicated),
		dedicatedProjectEnvironment:      "true",
		runIDEnvironment:                 "run-1234",
		namespaceEnvironment:             "gateway-api-openstack-e2e-run-1234",
		kubeconfigEnvironment:            "/workspace/kubeconfig",
		kubeContextEnvironment:           "e2e",
		controllerNameEnvironment:        "example.test/gateway-api-openstack",
		controllerNamespaceEnvironment:   "gateway-system",
		controllerDeploymentEnvironment:  "gateway-controller",
		controllerContainerEnvironment:   "controller",
		controllerReplicasEnvironment:    "2",
		controllerImageDigestEnvironment: digest,
		controllerRevisionEnvironment:    strings.Repeat("b", 40),
		leaderLeaseEnvironment:           "gateway-api-openstack-controller",
		backendImageEnvironment:          "registry.example.test/e2e/agnhost@" + digest,
		artifactDirectoryEnvironment:     "/workspace/artifacts/run-1234",
	}
}

func validSharedEnvironment() map[string]string {
	environment := validEnvironment()
	environment[projectModeEnvironment] = string(projectModeShared)
	environment[dedicatedProjectEnvironment] = "false"
	environment[controllerNameEnvironment] = "example.test/gao-e2e-run-1234"
	environment[controllerNamespaceEnvironment] = "openstack-gateway-e2e-run-1234"
	environment[auditEnableEnvironment] = "true"
	environment[auditBinaryEnvironment] = "/workspace/bin/openstack-gateway-audit"
	environment[clusterIDEnvironment] = "gao-e2e-run-1234"
	environment[auditCloudsYAMLEnvironment] = "/workspace/clouds.yaml"
	environment[auditCloudEnvironment] = "openstack-e2e"
	environment[auditRegionEnvironment] = "RegionOne"
	environment[expectedProjectIDEnvironment] = "expected-project"
	environment[expectedVIPSubnetIDEnvironment] = "expected-vip-subnet"
	environment[expectedMemberSubnetIDEnvironment] = "expected-member-subnet"
	environment[expectedExternalNetworkIDEnvironment] = sharedExternalNetworkNone
	environment[controllerCloudsSecretEnvironment] = "openstack-clouds-run-1234"
	return environment
}

func mapEnvironment(values map[string]string) environmentReader {
	return func(name string) string { return values[name] }
}
