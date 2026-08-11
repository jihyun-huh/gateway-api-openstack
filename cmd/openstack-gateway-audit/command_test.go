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
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/audit"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack"
)

const (
	testAuditController = "example.net/gateway-api-openstack"
	testAuditCluster    = "cluster-a"
	testAuditProject    = "project-a"
)

var testAuditTime = time.Date(2026, time.August, 10, 12, 30, 0, 0, time.FixedZone("test", 2*60*60))

func TestRunCommandExitStatus(t *testing.T) {
	tests := []struct {
		name       string
		extraArgs  []string
		inventory  audit.Inventory
		wantStatus int
	}{
		{
			name:       "complete report",
			wantStatus: exitSuccess,
		},
		{
			name: "findings do not fail by default",
			inventory: audit.Inventory{Resources: []audit.ResourceFinding{
				testOrphanFinding(),
			}},
			wantStatus: exitSuccess,
		},
		{
			name:      "findings use documented status when requested",
			extraArgs: []string{"--fail-on-findings"},
			inventory: audit.Inventory{Resources: []audit.ResourceFinding{
				testOrphanFinding(),
			}},
			wantStatus: exitFindings,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &staticAuditReader{}
			scanner := scannerFunc(func(context.Context, audit.Scope, []audit.OwnershipRecord) (audit.Inventory, error) {
				return test.inventory, nil
			})
			dependencies := successfulAuditDependencies(reader, scanner)
			stdout := &countingWriter{}
			var stderr bytes.Buffer

			status := runCommand(context.Background(), testAuditArgs(test.extraArgs...), emptyEnvironment, stdout, &stderr, dependencies)
			if status != test.wantStatus {
				t.Fatalf("runCommand() = %d, want %d; stderr: %s", status, test.wantStatus, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			if stdout.writes != 1 {
				t.Fatalf("stdout writes = %d, want one complete JSON write", stdout.writes)
			}
			var report audit.Report
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
				t.Fatalf("stdout is not a JSON report: %v\n%s", err, stdout.String())
			}
			if report.GeneratedAt != testAuditTime.UTC() {
				t.Errorf("generatedAt = %v, want %v", report.GeneratedAt, testAuditTime.UTC())
			}
			if got := report.HasFindings(); got != (len(test.inventory.Resources) != 0) {
				t.Errorf("report.HasFindings() = %t", got)
			}
			if !bytes.HasSuffix(stdout.Bytes(), []byte("\n")) {
				t.Error("stdout report does not end with a newline")
			}
		})
	}
}

func TestRunCommandHelpDoesNotInitializeDependencies(t *testing.T) {
	dependencies := commandDependencies{
		loadKubeConfig: func(commandOptions) (*rest.Config, error) {
			t.Fatal("loadKubeConfig called for --help")
			return nil, nil
		},
	}
	var stdout, stderr bytes.Buffer

	status := runCommand(context.Background(), []string{"--help"}, emptyEnvironment, &stdout, &stderr, dependencies)
	if status != exitSuccess {
		t.Fatalf("runCommand(--help) = %d, want %d", status, exitSuccess)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "openstack-gateway-audit [flags]") {
		t.Fatalf("help output = %q", stderr.String())
	}
}

func TestRunCommandStopsAtFirstFailureWithoutWritingStdout(t *testing.T) {
	secret := "token-and-provider-response-must-not-appear"
	tests := []struct {
		name        string
		failure     string
		wantTrace   []string
		wantMessage string
	}{
		{
			name:        "Kubernetes configuration",
			failure:     "load-kubeconfig",
			wantTrace:   []string{"load-kubeconfig"},
			wantMessage: "could not load Kubernetes client configuration",
		},
		{
			name:        "Kubernetes client",
			failure:     "new-kube-reader",
			wantTrace:   []string{"load-kubeconfig", "new-kube-reader"},
			wantMessage: "could not create Kubernetes API client",
		},
		{
			name:        "OpenStack clients",
			failure:     "new-service-clients",
			wantTrace:   []string{"load-kubeconfig", "new-kube-reader", "new-service-clients"},
			wantMessage: "could not create OpenStack service clients (Authentication)",
		},
		{
			name:    "Gateway list",
			failure: "list-gateways",
			wantTrace: []string{
				"load-kubeconfig", "new-kube-reader", "new-service-clients", "list-gateways",
			},
			wantMessage: "could not read Kubernetes ownership bindings",
		},
		{
			name:    "HTTPRoute list",
			failure: "list-routes",
			wantTrace: []string{
				"load-kubeconfig", "new-kube-reader", "new-service-clients", "list-gateways", "list-routes",
			},
			wantMessage: "could not read Kubernetes ownership bindings",
		},
		{
			name:    "OpenStack scan",
			failure: "scan",
			wantTrace: []string{
				"load-kubeconfig", "new-kube-reader", "new-service-clients", "list-gateways", "list-routes", "new-scanner", "scan",
			},
			wantMessage: "could not scan OpenStack ownership (RetryableService)",
		},
		{
			name:    "report construction",
			failure: "build-report",
			wantTrace: []string{
				"load-kubeconfig", "new-kube-reader", "new-service-clients", "list-gateways", "list-routes", "new-scanner", "scan", "now",
			},
			wantMessage: "could not build audit report",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var trace []string
			dependencies := stagedAuditDependencies(&trace, test.failure, secret)
			stdout := &countingWriter{}
			var stderr bytes.Buffer

			status := runCommand(context.Background(), testAuditArgs(), emptyEnvironment, stdout, &stderr, dependencies)
			if status != exitFailure {
				t.Fatalf("runCommand() = %d, want %d", status, exitFailure)
			}
			if stdout.writes != 0 || stdout.Len() != 0 {
				t.Fatalf("stdout received partial output after failure: writes=%d output=%q", stdout.writes, stdout.String())
			}
			if !slices.Equal(trace, test.wantTrace) {
				t.Fatalf("execution trace = %#v, want %#v", trace, test.wantTrace)
			}
			if !strings.Contains(stderr.String(), test.wantMessage) {
				t.Errorf("stderr = %q, want message containing %q", stderr.String(), test.wantMessage)
			}
			if strings.Contains(stderr.String(), secret) {
				t.Errorf("stderr exposes an underlying secret or provider response: %q", stderr.String())
			}
		})
	}
}

func TestRunCommandRedactsInvalidEnvironmentValue(t *testing.T) {
	secret := "application-credential-secret"
	getenv := func(name string) string {
		if name == "GATEWAY_OPENSTACK_API_QPS" {
			return secret
		}
		return ""
	}
	var stdout, stderr bytes.Buffer

	status := runCommand(context.Background(), testAuditArgs(), getenv, &stdout, &stderr, commandDependencies{})
	if status != exitFailure {
		t.Fatalf("runCommand() = %d, want %d", status, exitFailure)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if strings.Contains(stderr.String(), secret) {
		t.Fatalf("stderr exposes invalid environment value: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "GATEWAY_OPENSTACK_API_QPS must be a number") {
		t.Fatalf("stderr = %q, want fixed validation message", stderr.String())
	}
}

func TestRunCommandRedactsInvalidFlagValue(t *testing.T) {
	const secret = "private-flag-value"
	args := testAuditArgs("--openstack-api-qps=" + secret)
	var stdout, stderr bytes.Buffer

	status := runCommand(context.Background(), args, emptyEnvironment, &stdout, &stderr, commandDependencies{})
	if status != exitFailure {
		t.Fatalf("runCommand() = %d, want %d", status, exitFailure)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if stderr.String() != "error: invalid command options\n" {
		t.Fatalf("stderr = %q, want fixed option error", stderr.String())
	}
	if strings.Contains(stderr.String(), secret) {
		t.Fatalf("stderr exposes invalid flag value: %q", stderr.String())
	}
}

func TestExecuteAuditPropagatesContextAndScannerInputs(t *testing.T) {
	oldVersion := version
	version = "v0.2.0-test"
	t.Cleanup(func() { version = oldVersion })

	gateway := gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{
		Namespace: "gateways",
		Name:      "edge",
		UID:       "gateway-uid",
		Annotations: map[string]string{
			testBindingKey(testAuditController, "gateway-listener-port"): "80",
			testBindingKey(testAuditController, "gateway-cluster-id"):    testAuditCluster,
			testBindingKey(testAuditController, "gateway-project-id"):    testAuditProject,
		},
	}}
	route := gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Namespace: "apps",
		Name:      "app",
		UID:       "route-uid",
		Annotations: map[string]string{
			testBindingKey(testAuditController, "route-gateway-namespace"): gateway.Namespace,
			testBindingKey(testAuditController, "route-gateway-name"):      gateway.Name,
			testBindingKey(testAuditController, "route-gateway-uid"):       string(gateway.UID),
			testBindingKey(testAuditController, "route-cluster-id"):        testAuditCluster,
			testBindingKey(testAuditController, "route-project-id"):        testAuditProject,
		},
	}}
	type contextKey string
	const requestKey contextKey = "request"
	valueContext := context.WithValue(context.Background(), requestKey, "audit-request")
	ctx, cancel := context.WithCancel(valueContext)
	cancel()
	reader := &staticAuditReader{gateways: []gatewayv1.Gateway{gateway}, routes: []gatewayv1.HTTPRoute{route}}

	var serviceContext, scannerContext context.Context
	var scannerScope audit.Scope
	var scannerRecords []audit.OwnershipRecord
	var scannerTimeout time.Duration
	dependencies := commandDependencies{
		now: func() time.Time { return testAuditTime },
		loadKubeConfig: func(commandOptions) (*rest.Config, error) {
			return &rest.Config{Host: "https://kubernetes.example.invalid"}, nil
		},
		newKubeReader: func(*rest.Config) (client.Reader, error) { return reader, nil },
		newServiceClients: func(received context.Context, config openstack.ClientConfig) (openstack.ServiceClients, error) {
			serviceContext = received
			if config.Region != "region-one" || config.Microversion != "2.30" || config.APIQPS != 7 || config.APIBurst != 11 {
				t.Errorf("OpenStack client config = %#v, want parsed command options", config)
			}
			return openstack.ServiceClients{ProjectID: testAuditProject}, nil
		},
		newOpenStackScanner: func(_ openstack.ServiceClients, timeout time.Duration) audit.Scanner {
			scannerTimeout = timeout
			return scannerFunc(func(received context.Context, scope audit.Scope, records []audit.OwnershipRecord) (audit.Inventory, error) {
				scannerContext = received
				scannerScope = scope
				scannerRecords = append([]audit.OwnershipRecord(nil), records...)
				return audit.Inventory{}, nil
			})
		},
	}
	options := commandOptions{
		controllerName: testAuditController,
		clusterID:      testAuditCluster,
		openStack: openstack.ClientConfig{
			Region:       "region-one",
			Microversion: "2.30",
			APIQPS:       7,
			APIBurst:     11,
		},
		operationTimeout: 47 * time.Second,
	}

	report, err := executeAudit(ctx, options, dependencies)
	if err != nil {
		t.Fatalf("executeAudit() error = %v", err)
	}
	for name, received := range map[string]context.Context{
		"OpenStack client": serviceContext,
		"Gateway list":     reader.gatewayContext,
		"HTTPRoute list":   reader.routeContext,
		"OpenStack scan":   scannerContext,
	} {
		if received == nil || received.Value(requestKey) != "audit-request" {
			t.Errorf("%s did not receive the command context", name)
		}
		if !errors.Is(received.Err(), context.Canceled) {
			t.Errorf("%s did not receive command cancellation", name)
		}
	}
	if scannerTimeout != options.operationTimeout {
		t.Errorf("scanner timeout = %v, want %v", scannerTimeout, options.operationTimeout)
	}
	wantScope := audit.Scope{
		ClusterID:          testAuditCluster,
		ControllerName:     testAuditController,
		OpenStackProjectID: testAuditProject,
	}
	if scannerScope != wantScope {
		t.Errorf("scanner scope = %#v, want %#v", scannerScope, wantScope)
	}
	if len(scannerRecords) != 2 {
		t.Fatalf("scanner records = %#v, want Gateway and HTTPRoute", scannerRecords)
	}
	if scannerRecords[0].Objects[0].Kind != "Gateway" || scannerRecords[1].Objects[0].Kind != "HTTPRoute" {
		t.Errorf("scanner record order = %#v, want deterministic Gateway then HTTPRoute", scannerRecords)
	}
	for index, record := range scannerRecords {
		if record.Identity.Controller != testAuditController || record.Identity.ClusterID != testAuditCluster ||
			record.Identity.OpenStackProjectID != testAuditProject || record.Identity.ControllerVersion != version {
			t.Errorf("scanner record %d identity = %#v, want complete command scope", index, record.Identity)
		}
	}
	if report.Scope.ControllerName != testAuditController || report.Scope.ClusterID != testAuditCluster {
		t.Errorf("report scope = %#v", report.Scope)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal(report) error = %v", err)
	}
	if strings.Contains(string(encoded), testAuditProject) {
		t.Fatalf("report exposes authenticated project ID: %s", encoded)
	}
}

func TestRunCommandHandlesStdoutFailures(t *testing.T) {
	dependencies := successfulAuditDependencies(&staticAuditReader{}, scannerFunc(func(context.Context, audit.Scope, []audit.OwnershipRecord) (audit.Inventory, error) {
		return audit.Inventory{}, nil
	}))
	tests := []struct {
		name   string
		writer io.Writer
	}{
		{name: "write error", writer: failingWriter{err: errors.New("broken output")}},
		{name: "short write", writer: failingWriter{maximum: 3}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			status := runCommand(context.Background(), testAuditArgs(), emptyEnvironment, test.writer, &stderr, dependencies)
			if status != exitFailure {
				t.Fatalf("runCommand() = %d, want %d", status, exitFailure)
			}
			if stderr.String() != "error: could not write audit report\n" {
				t.Fatalf("stderr = %q, want fixed write error", stderr.String())
			}
		})
	}
}

func successfulAuditDependencies(reader client.Reader, scanner audit.Scanner) commandDependencies {
	return commandDependencies{
		now: func() time.Time { return testAuditTime },
		loadKubeConfig: func(commandOptions) (*rest.Config, error) {
			return &rest.Config{Host: "https://kubernetes.example.invalid"}, nil
		},
		newKubeReader: func(*rest.Config) (client.Reader, error) { return reader, nil },
		newServiceClients: func(context.Context, openstack.ClientConfig) (openstack.ServiceClients, error) {
			return openstack.ServiceClients{ProjectID: testAuditProject}, nil
		},
		newOpenStackScanner: func(openstack.ServiceClients, time.Duration) audit.Scanner { return scanner },
	}
}

func stagedAuditDependencies(trace *[]string, failure, secret string) commandDependencies {
	record := func(stage string) { *trace = append(*trace, stage) }
	reader := &staticAuditReader{
		onList: func(stage string) error {
			record(stage)
			if failure == stage {
				return errors.New(secret)
			}
			return nil
		},
	}
	return commandDependencies{
		now: func() time.Time {
			record("now")
			if failure == "build-report" {
				return time.Time{}
			}
			return testAuditTime
		},
		loadKubeConfig: func(commandOptions) (*rest.Config, error) {
			record("load-kubeconfig")
			if failure == "load-kubeconfig" {
				return nil, errors.New(secret)
			}
			return &rest.Config{Host: "https://kubernetes.example.invalid"}, nil
		},
		newKubeReader: func(*rest.Config) (client.Reader, error) {
			record("new-kube-reader")
			if failure == "new-kube-reader" {
				return nil, errors.New(secret)
			}
			return reader, nil
		},
		newServiceClients: func(context.Context, openstack.ClientConfig) (openstack.ServiceClients, error) {
			record("new-service-clients")
			if failure == "new-service-clients" {
				return openstack.ServiceClients{}, cloud.NewProviderError(cloud.ErrorCategoryAuthentication, errors.New(secret))
			}
			return openstack.ServiceClients{ProjectID: testAuditProject}, nil
		},
		newOpenStackScanner: func(openstack.ServiceClients, time.Duration) audit.Scanner {
			record("new-scanner")
			return scannerFunc(func(context.Context, audit.Scope, []audit.OwnershipRecord) (audit.Inventory, error) {
				record("scan")
				if failure == "scan" {
					return audit.Inventory{}, cloud.NewProviderError(cloud.ErrorCategoryRetryableService, errors.New(secret))
				}
				return audit.Inventory{}, nil
			})
		},
	}
}

func testAuditArgs(extra ...string) []string {
	args := []string{
		"--controller-name=" + testAuditController,
		"--cluster-id=" + testAuditCluster,
	}
	return append(args, extra...)
}

func emptyEnvironment(string) string { return "" }

func testOrphanFinding() audit.ResourceFinding {
	return audit.ResourceFinding{
		Service:     "octavia",
		Type:        "loadBalancer",
		ID:          "load-balancer-id",
		Disposition: audit.DispositionOrphanCandidate,
		Reason:      "NoCurrentBinding",
		Message:     "No current Kubernetes binding matches this resource",
		Objects:     []audit.ObjectReference{},
	}
}

func testBindingKey(controllerName, name string) string {
	domain := strings.SplitN(controllerName, "/", 2)[0]
	digest := sha256.Sum256([]byte(controllerName))
	return fmt.Sprintf("%s/%s-%x", domain, name, digest[:6])
}

type scannerFunc func(context.Context, audit.Scope, []audit.OwnershipRecord) (audit.Inventory, error)

func (f scannerFunc) Scan(ctx context.Context, scope audit.Scope, records []audit.OwnershipRecord) (audit.Inventory, error) {
	return f(ctx, scope, records)
}

type staticAuditReader struct {
	gateways       []gatewayv1.Gateway
	routes         []gatewayv1.HTTPRoute
	gatewayContext context.Context
	routeContext   context.Context
	onList         func(string) error
}

func (r *staticAuditReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("unexpected Get call")
}

func (r *staticAuditReader) List(ctx context.Context, list client.ObjectList, _ ...client.ListOption) error {
	switch typed := list.(type) {
	case *gatewayv1.GatewayList:
		r.gatewayContext = ctx
		if r.onList != nil {
			if err := r.onList("list-gateways"); err != nil {
				return err
			}
		}
		typed.Items = append([]gatewayv1.Gateway(nil), r.gateways...)
		return nil
	case *gatewayv1.HTTPRouteList:
		r.routeContext = ctx
		if r.onList != nil {
			if err := r.onList("list-routes"); err != nil {
				return err
			}
		}
		typed.Items = append([]gatewayv1.HTTPRoute(nil), r.routes...)
		return nil
	default:
		return fmt.Errorf("unexpected list type %T", list)
	}
}

type countingWriter struct {
	bytes.Buffer
	writes int
}

func (w *countingWriter) Write(value []byte) (int, error) {
	w.writes++
	return w.Buffer.Write(value)
}

type failingWriter struct {
	maximum int
	err     error
}

func (w failingWriter) Write(value []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.maximum < len(value) {
		return w.maximum, nil
	}
	return len(value), nil
}
