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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
)

func TestParseOptionsFlagsOverrideEnvironment(t *testing.T) {
	environment := map[string]string{
		"GATEWAY_OPENSTACK_CONTROLLER_NAME": "env.example.net/controller",
		"GATEWAY_OPENSTACK_CLUSTER_ID":      "environment-cluster",
		"GATEWAY_OPENSTACK_API_QPS":         "3.5",
		"GATEWAY_OPENSTACK_API_BURST":       "4",
		"OS_REGION_NAME":                    "environment-region",
		"OS_CLIENT_CONFIG_FILE":             "/environment/clouds.yaml",
		"OS_CLOUD":                          "environment-cloud",
	}
	getenv := func(name string) string { return environment[name] }
	args := []string{
		"--controller-name=flags.example.net/controller",
		"--cluster-id=flag-cluster",
		"--kubeconfig=/flag/kubeconfig",
		"--context=flag-context",
		"--openstack-region=flag-region",
		"--octavia-microversion=2.30",
		"--clouds-yaml=/flag/clouds.yaml",
		"--openstack-cloud=flag-cloud",
		"--insecure",
		"--openstack-api-qps=8.5",
		"--openstack-api-burst=9",
		"--openstack-operation-timeout=37s",
		"--fail-on-findings",
	}

	options, err := parseOptions(args, getenv, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseOptions() error = %v", err)
	}
	if options.controllerName != "flags.example.net/controller" {
		t.Errorf("controller name = %q, want flag value", options.controllerName)
	}
	if options.clusterID != "flag-cluster" {
		t.Errorf("cluster ID = %q, want flag value", options.clusterID)
	}
	if options.kubeconfig != "/flag/kubeconfig" || options.kubeContext != "flag-context" {
		t.Errorf("Kubernetes selection = %q, %q, want flag values", options.kubeconfig, options.kubeContext)
	}
	if options.openStack.Region != "flag-region" || options.openStack.Microversion != "2.30" {
		t.Errorf("OpenStack region and microversion = %q, %q, want flag values", options.openStack.Region, options.openStack.Microversion)
	}
	if options.openStack.CloudsYAMLPath != "/flag/clouds.yaml" || options.openStack.CloudName != "flag-cloud" {
		t.Errorf("cloud selection = %q, %q, want flag values", options.openStack.CloudsYAMLPath, options.openStack.CloudName)
	}
	if !options.openStack.AllowInsecure {
		t.Error("AllowInsecure = false, want true")
	}
	if options.openStack.APIQPS != 8.5 || options.openStack.APIBurst != 9 {
		t.Errorf("OpenStack limits = %v, %d, want 8.5, 9", options.openStack.APIQPS, options.openStack.APIBurst)
	}
	if options.operationTimeout != 37*time.Second || !options.failOnFindings {
		t.Errorf("audit behavior = %v, %t, want 37s, true", options.operationTimeout, options.failOnFindings)
	}
}

func TestParseOptionsValidFlagsOverrideMalformedNumericEnvironment(t *testing.T) {
	environment := map[string]string{
		"GATEWAY_OPENSTACK_API_QPS":   "not-a-number",
		"GATEWAY_OPENSTACK_API_BURST": "not-an-integer",
	}
	options, err := parseOptions([]string{
		"--controller-name=example.net/controller",
		"--cluster-id=cluster-a",
		"--openstack-api-qps=7.5",
		"--openstack-api-burst=8",
	}, func(name string) string { return environment[name] }, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseOptions() error = %v, want explicit flags to bypass malformed defaults", err)
	}
	if options.openStack.APIQPS != 7.5 || options.openStack.APIBurst != 8 {
		t.Fatalf("OpenStack limits = %v, %d, want flag values", options.openStack.APIQPS, options.openStack.APIBurst)
	}
}

func TestParseOptionsUsesEnvironmentDefaults(t *testing.T) {
	environment := map[string]string{
		"GATEWAY_OPENSTACK_CONTROLLER_NAME": "env.example.net/controller",
		"GATEWAY_OPENSTACK_CLUSTER_ID":      "environment-cluster",
		"GATEWAY_OPENSTACK_API_QPS":         "3.5",
		"GATEWAY_OPENSTACK_API_BURST":       "4",
		"OS_REGION_NAME":                    "environment-region",
		"OS_CLIENT_CONFIG_FILE":             "/environment/clouds.yaml",
		"OS_CLOUD":                          "environment-cloud",
	}
	getenv := func(name string) string { return environment[name] }

	options, err := parseOptions(nil, getenv, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseOptions() error = %v", err)
	}
	if options.controllerName != environment["GATEWAY_OPENSTACK_CONTROLLER_NAME"] ||
		options.clusterID != environment["GATEWAY_OPENSTACK_CLUSTER_ID"] {
		t.Errorf("controller scope = %q, %q, want environment values", options.controllerName, options.clusterID)
	}
	if options.openStack.Region != environment["OS_REGION_NAME"] ||
		options.openStack.CloudsYAMLPath != environment["OS_CLIENT_CONFIG_FILE"] ||
		options.openStack.CloudName != environment["OS_CLOUD"] {
		t.Errorf("cloud selection = %#v, want environment values", options.openStack)
	}
	if options.openStack.APIQPS != 3.5 || options.openStack.APIBurst != 4 {
		t.Errorf("OpenStack limits = %v, %d, want 3.5, 4", options.openStack.APIQPS, options.openStack.APIBurst)
	}
	if options.openStack.Microversion != "2.5" || options.operationTimeout != 10*time.Minute {
		t.Errorf("defaults = microversion %q, timeout %v, want 2.5 and 10m", options.openStack.Microversion, options.operationTimeout)
	}
}

func TestParseOptionsValidation(t *testing.T) {
	valid := []string{"--controller-name=example.net/controller", "--cluster-id=cluster-a"}
	tests := []struct {
		name        string
		args        []string
		environment map[string]string
		want        string
	}{
		{name: "missing controller name", args: []string{"--cluster-id=cluster-a"}, want: "controller name must be a domain prefixed path"},
		{name: "invalid controller name", args: []string{"--controller-name=controller", "--cluster-id=cluster-a"}, want: "controller name must be a domain prefixed path"},
		{name: "empty cluster ID", args: []string{"--controller-name=example.net/controller", "--cluster-id=   "}, want: "cluster ID must not be empty"},
		{name: "positional argument", args: append(append([]string{}, valid...), "extra"), want: "positional arguments are not supported"},
		{name: "zero operation timeout", args: append(append([]string{}, valid...), "--openstack-operation-timeout=0"), want: "operation timeout for OpenStack must be greater than zero"},
		{name: "negative operation timeout", args: append(append([]string{}, valid...), "--openstack-operation-timeout=-1s"), want: "operation timeout for OpenStack must be greater than zero"},
		{name: "old microversion", args: append(append([]string{}, valid...), "--octavia-microversion=2.4"), want: "invalid OpenStack client configuration"},
		{name: "zero QPS", args: append(append([]string{}, valid...), "--openstack-api-qps=0"), want: "invalid OpenStack client configuration"},
		{name: "NaN QPS", args: append(append([]string{}, valid...), "--openstack-api-qps=NaN"), want: "invalid OpenStack client configuration"},
		{name: "zero burst", args: append(append([]string{}, valid...), "--openstack-api-burst=0"), want: "invalid OpenStack client configuration"},
		{name: "invalid QPS environment", args: valid, environment: map[string]string{"GATEWAY_OPENSTACK_API_QPS": "credential-value"}, want: "GATEWAY_OPENSTACK_API_QPS must be a number"},
		{name: "invalid burst environment", args: valid, environment: map[string]string{"GATEWAY_OPENSTACK_API_BURST": "credential-value"}, want: "GATEWAY_OPENSTACK_API_BURST must be an integer"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			getenv := func(name string) string { return test.environment[name] }
			_, err := parseOptions(test.args, getenv, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parseOptions() error = %v, want message containing %q", err, test.want)
			}
			if strings.Contains(err.Error(), "credential-value") {
				t.Fatalf("parseOptions() error exposes environment value: %v", err)
			}
		})
	}
}

func TestParseOptionsHelp(t *testing.T) {
	environment := map[string]string{
		"GATEWAY_OPENSTACK_CONTROLLER_NAME": "sentinel-controller.example/secret",
		"GATEWAY_OPENSTACK_CLUSTER_ID":      "sentinel-cluster",
		"GATEWAY_OPENSTACK_API_QPS":         "123.45",
		"GATEWAY_OPENSTACK_API_BURST":       "123",
		"OS_REGION_NAME":                    "sentinel-region",
		"OS_CLIENT_CONFIG_FILE":             "/sentinel/private/clouds.yaml",
		"OS_CLOUD":                          "sentinel-cloud",
	}
	for _, argument := range []string{"--help", "-h"} {
		t.Run(argument, func(t *testing.T) {
			var output bytes.Buffer
			_, err := parseOptions([]string{argument}, func(name string) string { return environment[name] }, &output)
			if err != pflag.ErrHelp {
				t.Fatalf("parseOptions() error = %v, want pflag.ErrHelp", err)
			}
			for _, text := range []string{
				"Read-only Kubernetes and OpenStack ownership audit",
				"openstack-gateway-audit [flags]",
				"--controller-name",
				"--fail-on-findings",
			} {
				if !strings.Contains(output.String(), text) {
					t.Errorf("help output does not contain %q:\n%s", text, output.String())
				}
			}
			for _, value := range environment {
				if strings.Contains(output.String(), value) {
					t.Errorf("help output exposes environment value %q:\n%s", value, output.String())
				}
			}
		})
	}
}

func TestLoadKubernetesConfigUsesExplicitContext(t *testing.T) {
	kubeconfig := `apiVersion: v1
kind: Config
clusters:
- name: first-cluster
  cluster:
    server: https://first.example.invalid
- name: second-cluster
  cluster:
    server: https://second.example.invalid
users:
- name: first-user
  user:
    token: first-token
- name: second-user
  user:
    token: second-token
contexts:
- name: first
  context:
    cluster: first-cluster
    user: first-user
- name: second
  context:
    cluster: second-cluster
    user: second-user
current-context: first
`
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	config, err := loadKubernetesConfig(commandOptions{kubeconfig: path, kubeContext: "second"})
	if err != nil {
		t.Fatalf("loadKubernetesConfig() error = %v", err)
	}
	if config.Host != "https://second.example.invalid" || config.BearerToken != "second-token" {
		t.Fatalf("loadKubernetesConfig() selected host %q and token %q, want second context", config.Host, config.BearerToken)
	}

	_, err = loadKubernetesConfig(commandOptions{kubeconfig: path, kubeContext: "missing"})
	if err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("loadKubernetesConfig() missing-context error = %v", err)
	}
}
