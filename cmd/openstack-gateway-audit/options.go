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
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack"
)

type commandOptions struct {
	controllerName   string
	clusterID        string
	kubeconfig       string
	kubeContext      string
	openStack        openstack.ClientConfig
	operationTimeout time.Duration
	failOnFindings   bool
}

func parseOptions(args []string, getenv func(string) string, stderr io.Writer) (commandOptions, error) {
	options := commandOptions{
		openStack: openstack.ClientConfig{
			APIQPS:   openstack.DefaultAPIQPS,
			APIBurst: openstack.DefaultAPIBurst,
		},
	}
	flags := pflag.NewFlagSet("openstack-gateway-audit", pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.controllerName, "controller-name", "", "Gateway API controller name (required; defaults to GATEWAY_OPENSTACK_CONTROLLER_NAME)")
	flags.StringVar(&options.clusterID, "cluster-id", "", "stable identifier for this Kubernetes cluster (required; defaults to GATEWAY_OPENSTACK_CLUSTER_ID)")
	flags.StringVar(&options.kubeconfig, "kubeconfig", "", "path to a kubeconfig file; defaults to standard client-go loading")
	flags.StringVar(&options.kubeContext, "context", "", "kubeconfig context to use")
	flags.StringVar(&options.openStack.Region, "openstack-region", "", "OpenStack region; defaults to clouds.yaml or OS_REGION_NAME")
	flags.StringVar(&options.openStack.Microversion, "octavia-microversion", "2.5", "Octavia API microversion; 2.5 or newer is required")
	flags.StringVar(&options.openStack.CloudsYAMLPath, "clouds-yaml", "", "path to clouds.yaml; defaults to OS_CLIENT_CONFIG_FILE")
	flags.StringVar(&options.openStack.CloudName, "openstack-cloud", "", "clouds.yaml entry; defaults to OS_CLOUD")
	flags.BoolVar(&options.openStack.AllowInsecure, "insecure", false, "disable OpenStack TLS verification; test clouds only")
	flags.Float64Var(&options.openStack.APIQPS, "openstack-api-qps", openstack.DefaultAPIQPS, "maximum average OpenStack API requests per second; defaults to GATEWAY_OPENSTACK_API_QPS")
	flags.IntVar(&options.openStack.APIBurst, "openstack-api-burst", openstack.DefaultAPIBurst, "maximum burst of OpenStack API requests; defaults to GATEWAY_OPENSTACK_API_BURST")
	flags.DurationVar(&options.operationTimeout, "openstack-operation-timeout", 10*time.Minute, "maximum duration of the OpenStack inventory scan")
	flags.BoolVar(&options.failOnFindings, "fail-on-findings", false, "exit with status 2 after writing a report that contains findings")
	if err := flags.Parse(args); err != nil {
		if err == pflag.ErrHelp {
			printUsage(stderr, flags)
			return commandOptions{}, err
		}
		return commandOptions{}, newCommandError("invalid command options", err)
	}
	if flags.NArg() != 0 {
		return commandOptions{}, newCommandError("positional arguments are not supported", nil)
	}
	if err := applyOptionEnvironment(&options, flags, getenv); err != nil {
		return commandOptions{}, err
	}
	if len(options.controllerName) > 253 || len(validation.IsDomainPrefixedPath(field.NewPath("controller-name"), options.controllerName)) != 0 {
		return commandOptions{}, newCommandError("controller name must be a domain prefixed path", nil)
	}
	if strings.TrimSpace(options.clusterID) == "" {
		return commandOptions{}, newCommandError("cluster ID must not be empty", nil)
	}
	if options.operationTimeout <= 0 {
		return commandOptions{}, newCommandError("operation timeout for OpenStack must be greater than zero", nil)
	}
	if err := options.openStack.Validate(); err != nil {
		return commandOptions{}, newCommandError("invalid OpenStack client configuration", err)
	}
	return options, nil
}

func applyOptionEnvironment(options *commandOptions, flags *pflag.FlagSet, getenv func(string) string) error {
	setFlags := make(map[string]struct{})
	flags.Visit(func(value *pflag.Flag) {
		setFlags[value.Name] = struct{}{}
	})
	if _, set := setFlags["controller-name"]; !set {
		options.controllerName = getenv("GATEWAY_OPENSTACK_CONTROLLER_NAME")
	}
	if _, set := setFlags["cluster-id"]; !set {
		options.clusterID = getenv("GATEWAY_OPENSTACK_CLUSTER_ID")
	}
	if _, set := setFlags["openstack-region"]; !set {
		options.openStack.Region = getenv("OS_REGION_NAME")
	}
	if _, set := setFlags["clouds-yaml"]; !set {
		options.openStack.CloudsYAMLPath = getenv("OS_CLIENT_CONFIG_FILE")
	}
	if _, set := setFlags["openstack-cloud"]; !set {
		options.openStack.CloudName = getenv("OS_CLOUD")
	}
	if _, set := setFlags["openstack-api-qps"]; !set {
		value, err := auditFloat64EnvOr(getenv, "GATEWAY_OPENSTACK_API_QPS", openstack.DefaultAPIQPS)
		if err != nil {
			return err
		}
		options.openStack.APIQPS = value
	}
	if _, set := setFlags["openstack-api-burst"]; !set {
		value, err := auditIntEnvOr(getenv, "GATEWAY_OPENSTACK_API_BURST", openstack.DefaultAPIBurst)
		if err != nil {
			return err
		}
		options.openStack.APIBurst = value
	}
	return nil
}

func printUsage(writer io.Writer, flags *pflag.FlagSet) {
	previousOutput := flags.Output()
	flags.SetOutput(writer)
	defer flags.SetOutput(previousOutput)
	_, _ = fmt.Fprintln(writer, `Read-only Kubernetes and OpenStack ownership audit

Usage:
  openstack-gateway-audit [flags]

The command writes one experimental JSON report to stdout. It never adopts,
changes, or deletes resources. The report contains local identifiers and must
be reviewed before it is shared.`)
	flags.PrintDefaults()
}

func auditFloat64EnvOr(getenv func(string) string, name string, fallback float64) (float64, error) {
	value := getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, newCommandError(name+" must be a number", err)
	}
	return parsed, nil
}

func auditIntEnvOr(getenv func(string) string, name string, fallback int) (int, error) {
	value := getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, newCommandError(name+" must be an integer", err)
	}
	return parsed, nil
}
