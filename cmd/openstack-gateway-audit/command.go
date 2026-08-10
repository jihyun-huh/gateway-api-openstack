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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/audit"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
)

const (
	exitSuccess  = 0
	exitFailure  = 1
	exitFindings = 2
)

type commandDependencies struct {
	now                 func() time.Time
	loadKubeConfig      func(commandOptions) (*rest.Config, error)
	newKubeReader       func(*rest.Config) (client.Reader, error)
	newServiceClients   func(context.Context, openstack.ClientConfig) (openstack.ServiceClients, error)
	newOpenStackScanner func(openstack.ServiceClients, time.Duration) audit.Scanner
}

func productionDependencies() commandDependencies {
	return commandDependencies{
		now:               time.Now,
		loadKubeConfig:    loadKubernetesConfig,
		newKubeReader:     newKubernetesReader,
		newServiceClients: openstack.NewServiceClients,
		newOpenStackScanner: func(clients openstack.ServiceClients, operationTimeout time.Duration) audit.Scanner {
			return openstack.NewProvider(clients, openstack.ProviderConfig{OperationTimeout: operationTimeout})
		},
	}
}

func runCommand(
	ctx context.Context,
	args []string,
	getenv func(string) string,
	stdout io.Writer,
	stderr io.Writer,
	dependencies commandDependencies,
) int {
	options, err := parseOptions(args, getenv, stderr)
	if errors.Is(err, pflag.ErrHelp) {
		return exitSuccess
	}
	if err != nil {
		writeCommandError(stderr, err)
		return exitFailure
	}

	report, err := executeAudit(ctx, options, dependencies)
	if err != nil {
		writeCommandError(stderr, err)
		return exitFailure
	}
	output, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		writeCommandError(stderr, newCommandError("could not encode audit report", err))
		return exitFailure
	}
	output = append(output, '\n')
	written, err := stdout.Write(output)
	if err != nil {
		writeCommandError(stderr, newCommandError("could not write audit report", err))
		return exitFailure
	}
	if written != len(output) {
		writeCommandError(stderr, newCommandError("could not write audit report", io.ErrShortWrite))
		return exitFailure
	}
	if options.failOnFindings && report.HasFindings() {
		return exitFindings
	}
	return exitSuccess
}

func executeAudit(ctx context.Context, options commandOptions, dependencies commandDependencies) (audit.Report, error) {
	restConfig, err := dependencies.loadKubeConfig(options)
	if err != nil {
		return audit.Report{}, newCommandError("could not load Kubernetes client configuration", err)
	}
	reader, err := dependencies.newKubeReader(restConfig)
	if err != nil {
		return audit.Report{}, newCommandError("could not create Kubernetes API client", err)
	}
	serviceClients, err := dependencies.newServiceClients(ctx, options.openStack)
	if err != nil {
		return audit.Report{}, newOpenStackCommandError("could not create OpenStack service clients", err)
	}

	controllerConfig := controller.Config{
		ControllerName:     gatewayv1.GatewayController(options.controllerName),
		ControllerVersion:  version,
		ClusterID:          options.clusterID,
		OpenStackProjectID: serviceClients.ProjectID,
	}
	snapshot, err := controller.CollectOwnershipSnapshot(ctx, reader, controllerConfig)
	if err != nil {
		return audit.Report{}, newCommandError("could not read Kubernetes ownership bindings", err)
	}
	scope := audit.Scope{
		ClusterID:          options.clusterID,
		ControllerName:     options.controllerName,
		OpenStackProjectID: serviceClients.ProjectID,
	}
	scanner := dependencies.newOpenStackScanner(serviceClients, options.operationTimeout)
	inventory, err := scanner.Scan(ctx, scope, snapshot.Records)
	if err != nil {
		return audit.Report{}, newOpenStackCommandError("could not scan OpenStack ownership", err)
	}
	report, err := audit.BuildReport(dependencies.now(), version, scope, snapshot, inventory)
	if err != nil {
		return audit.Report{}, newCommandError("could not build audit report", err)
	}
	return report, nil
}

func loadKubernetesConfig(options commandOptions) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if options.kubeconfig != "" {
		rules.ExplicitPath = options.kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: options.kubeContext}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
}

func newKubernetesReader(config *rest.Config) (client.Reader, error) {
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		return nil, fmt.Errorf("install Gateway API scheme: %w", err)
	}
	return client.New(config, client.Options{Scheme: scheme})
}

type commandError struct {
	message string
	cause   error
}

func newCommandError(message string, cause error) error {
	return &commandError{message: message, cause: cause}
}

func newOpenStackCommandError(message string, cause error) error {
	if category, ok := cloud.ErrorCategoryOf(cause); ok {
		message += " (" + string(category) + ")"
	}
	return newCommandError(message, cause)
}

func (e *commandError) Error() string { return e.message }
func (e *commandError) Unwrap() error { return e.cause }

func writeCommandError(writer io.Writer, err error) {
	_, _ = fmt.Fprintf(writer, "error: %s\n", err)
}
