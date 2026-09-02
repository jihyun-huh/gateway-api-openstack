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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/audit"
)

func TestLoadAndResolveExampleConfig(t *testing.T) {
	repositoryRoot := testRepositoryRoot(t)
	path, err := filepath.Abs(filepath.Join("..", "shared-project.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	file, err := loadFileConfig(path)
	if err != nil {
		t.Fatalf("loadFileConfig() error = %v", err)
	}
	config, err := resolveFileConfig(file, resolveOptions{
		repositoryRoot: repositoryRoot,
		now:            func() time.Time { return time.Date(2026, 9, 1, 12, 34, 56, 0, time.UTC) },
		random:         bytes.NewReader([]byte{0xaa, 0xbb, 0xcc, 0xdd}),
	})
	if err != nil {
		t.Fatalf("resolveFileConfig() error = %v", err)
	}
	if config.runID != "run-20260901-123456-aabbccdd" || config.workloadNamespace != "gateway-api-openstack-e2e-run-20260901-123456-aabbccdd" ||
		config.controllerNamespace != "openstack-gateway-e2e-run-20260901-123456-aabbccdd" || config.gatewayClassName != "gao-e2e-run-20260901-123456-aabbccdd" ||
		config.controllerName != "e2e.example.test/gao-e2e-run-20260901-123456-aabbccdd" || config.clusterID != "gao-e2e-run-20260901-123456-aabbccdd" {
		t.Fatalf("resolved identity = %#v", config)
	}
	if config.externalNetworkInput != disabledNetwork || config.externalNetworkID != "" {
		t.Fatalf("external network input = %q, resolved = %q", config.externalNetworkInput, config.externalNetworkID)
	}
	if config.controllerImageDigest != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("controller image digest = %q", config.controllerImageDigest)
	}
}

func TestLoadFileConfigIsStrict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := []byte(`formatVersion: v1alpha1
unknownField: true
`)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFileConfig(path); err == nil || !strings.Contains(err.Error(), "unknownField") {
		t.Fatalf("loadFileConfig() error = %v, want strict unknown-field rejection", err)
	}
}

func TestResolveFileConfigDerivesRunIdentityAndAuditDefault(t *testing.T) {
	file := validFileConfig()
	file.RunID = ""
	file.OpenStack.AuditCloudsYAML = ""
	config, err := resolveFileConfig(file, resolveOptions{
		repositoryRoot: testRepositoryRoot(t),
		now:            func() time.Time { return time.Date(2026, 9, 1, 12, 34, 56, 0, time.UTC) },
		random:         bytes.NewReader([]byte{0xaa, 0xbb, 0xcc, 0xdd}),
	})
	if err != nil {
		t.Fatalf("resolveFileConfig() error = %v", err)
	}
	if config.runID != "run-20260901-123456-aabbccdd" {
		t.Fatalf("runID = %q", config.runID)
	}
	if config.auditCloudsYAML != file.OpenStack.ControllerCloudsYAML {
		t.Fatalf("audit clouds path = %q, want controller path", config.auditCloudsYAML)
	}
}

func TestResolveFileConfigRejectsMissingSafetyInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fileConfig)
		want   string
	}{
		{name: "Kubernetes is not dedicated", mutate: func(config *fileConfig) { config.Kubernetes.DedicatedForE2E = false }, want: "dedicatedForE2E"},
		{name: "project mode is not shared", mutate: func(config *fileConfig) { config.OpenStack.ProjectMode = "dedicated" }, want: "projectMode"},
		{name: "project credential risk is not accepted", mutate: func(config *fileConfig) { config.OpenStack.AcceptProjectWideCredentialRisk = false }, want: "acceptProjectWideCredentialRisk"},
		{name: "external network choice is omitted", mutate: func(config *fileConfig) { config.OpenStack.ExternalNetworkID = "" }, want: "externalNetworkID"},
		{name: "controller image is mutable", mutate: func(config *fileConfig) { config.Controller.Image = "registry.example.test/controller:latest" }, want: "controller.image"},
		{name: "backend image is mutable", mutate: func(config *fileConfig) { config.Backend.Image = "registry.example.test/agnhost:latest" }, want: "backend.image"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validFileConfig()
			test.mutate(&config)
			_, err := resolveFileConfig(config, resolveOptions{repositoryRoot: testRepositoryRoot(t)})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("resolveFileConfig() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestBuildControllerObjectsUsesRunScopedIdentity(t *testing.T) {
	repositoryRoot := testRepositoryRoot(t)
	base, err := loadBaseObjects(repositoryRoot)
	if err != nil {
		t.Fatalf("loadBaseObjects() error = %v", err)
	}
	config, err := resolveFileConfig(validFileConfig(), resolveOptions{repositoryRoot: repositoryRoot})
	if err != nil {
		t.Fatal(err)
	}
	objects, err := buildControllerObjects(base, config, []byte("clouds: {}\n"))
	if err != nil {
		t.Fatalf("buildControllerObjects() error = %v", err)
	}
	for name, object := range map[string]interface {
		GetAnnotations() map[string]string
	}{
		"Namespace":          objects.namespace,
		"ServiceAccount":     objects.serviceAccount,
		"ClusterRole":        objects.clusterRole,
		"ClusterRoleBinding": objects.clusterRoleBinding,
		"ConfigMap":          objects.configMap,
		"Secret":             objects.secret,
		"Deployment":         objects.deployment,
	} {
		if object.GetAnnotations()[runAnnotation] != config.runID {
			t.Errorf("%s run annotation = %q", name, object.GetAnnotations()[runAnnotation])
		}
	}
	if objects.namespace.Name != config.controllerNamespace || objects.serviceAccount.Namespace != config.controllerNamespace ||
		objects.clusterRole.Name != config.clusterRoleName || objects.clusterRoleBinding.RoleRef.Name != config.clusterRoleName ||
		len(objects.clusterRoleBinding.Subjects) != 1 || objects.clusterRoleBinding.Subjects[0].Namespace != config.controllerNamespace {
		t.Fatalf("run-scoped RBAC objects = %#v", objects)
	}
	if objects.configMap.Immutable == nil || !*objects.configMap.Immutable || objects.secret.Immutable == nil || !*objects.secret.Immutable {
		t.Fatal("ConfigMap or Secret is mutable")
	}
	for name, want := range map[string]string{
		"GATEWAY_OPENSTACK_CONTROLLER_NAME":     config.controllerName,
		"GATEWAY_OPENSTACK_CLUSTER_ID":          config.clusterID,
		"GATEWAY_OPENSTACK_OCTAVIA_PROVIDER":    "amphora",
		"GATEWAY_OPENSTACK_VIP_SUBNET_ID":       config.vipSubnetID,
		"GATEWAY_OPENSTACK_MEMBER_SUBNET_ID":    config.memberSubnetID,
		"GATEWAY_OPENSTACK_EXTERNAL_NETWORK_ID": "",
		"GATEWAY_OPENSTACK_MEMBER_MODE":         "NodePort",
		"GATEWAY_OPENSTACK_NODE_ADDRESS_TYPE":   "InternalIP",
		"OS_CLIENT_CONFIG_FILE":                 "/etc/openstack/clouds.yaml",
		"OS_CLOUD":                              config.cloud,
		"OS_REGION_NAME":                        config.region,
	} {
		if got := objects.configMap.Data[name]; got != want {
			t.Errorf("ConfigMap %s = %q, want %q", name, got, want)
		}
	}
	container := objects.deployment.Spec.Template.Spec.Containers[0]
	if container.Name != controllerContainerName || container.Image != config.controllerImage || len(container.Command) != 0 || len(container.Env) != 0 ||
		len(container.EnvFrom) != 1 || container.EnvFrom[0].ConfigMapRef == nil || container.EnvFrom[0].ConfigMapRef.Name != objects.configMap.Name {
		t.Fatalf("controller container = %#v", container)
	}
	if len(container.VolumeMounts) != 1 || container.VolumeMounts[0].MountPath != controllerCloudsMountPath || !container.VolumeMounts[0].ReadOnly ||
		len(objects.deployment.Spec.Template.Spec.Volumes) != 1 || objects.deployment.Spec.Template.Spec.Volumes[0].Secret == nil ||
		objects.deployment.Spec.Template.Spec.Volumes[0].Secret.SecretName != objects.secret.Name {
		t.Fatalf("controller clouds mount = %#v, volumes = %#v", container.VolumeMounts, objects.deployment.Spec.Template.Spec.Volumes)
	}
	for _, argument := range []string{"--leader-elect=true", "--metrics-bind-address=:8080", "--octavia-microversion=2.5"} {
		if !contains(container.Args, argument) {
			t.Errorf("controller args %q do not contain %q", container.Args, argument)
		}
	}
	metricsPorts := 0
	for _, port := range container.Ports {
		if port.Name == "metrics" && port.ContainerPort == 8080 && port.Protocol == corev1.ProtocolTCP {
			metricsPorts++
		}
	}
	if metricsPorts != 1 {
		t.Fatalf("metrics port count = %d, ports = %#v", metricsPorts, container.Ports)
	}

	objects.configMap.UID = types.UID("config-uid")
	objects.configMap.ResourceVersion = "12"
	objects.secret.UID = types.UID("secret-uid")
	objects.secret.ResourceVersion = "13"
	if err := bindDeploymentSources(objects.deployment, objects.configMap, objects.secret); err != nil {
		t.Fatal(err)
	}
	if got := objects.deployment.Spec.Template.Annotations[controllerConfigSourceAnnotation]; got != "config-uid/12" {
		t.Fatalf("config source annotation = %q", got)
	}
	if got := objects.deployment.Spec.Template.Annotations[controllerCloudsSourceAnnotation]; got != "secret-uid/13" {
		t.Fatalf("clouds source annotation = %q", got)
	}

	base.deployment.Spec.Template.Spec.Containers[0].Args = append(
		base.deployment.Spec.Template.Spec.Containers[0].Args,
		"--controller-name=foreign.example.test/controller",
	)
	if _, err := buildControllerObjects(base, config, []byte("clouds: {}\n")); err == nil || !strings.Contains(err.Error(), "protected setting") {
		t.Fatalf("buildControllerObjects() accepted a protected argument override: %v", err)
	}
}

func TestHarnessEnvironmentUsesResolvedValuesAndDropsInheritedOverrides(t *testing.T) {
	config, err := resolveFileConfig(validFileConfig(), resolveOptions{repositoryRoot: testRepositoryRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	environment := config.harnessEnvironment()
	for name, want := range map[string]string{
		"GATEWAY_OPENSTACK_E2E_PROJECT_MODE":                 "shared",
		"GATEWAY_OPENSTACK_E2E_RUN_ID":                       config.runID,
		"GATEWAY_OPENSTACK_E2E_NAMESPACE":                    config.workloadNamespace,
		"GATEWAY_OPENSTACK_E2E_CONTROLLER_NAME":              config.controllerName,
		"GATEWAY_OPENSTACK_E2E_CLUSTER_ID":                   config.clusterID,
		"GATEWAY_OPENSTACK_E2E_EXPECTED_EXTERNAL_NETWORK_ID": "none",
		"GATEWAY_OPENSTACK_E2E_CONTROLLER_IMAGE_DIGEST":      config.controllerImageDigest,
		"GATEWAY_OPENSTACK_E2E_AUDIT":                        "true",
	} {
		if got := environment[name]; got != want {
			t.Errorf("environment[%s] = %q, want %q", name, got, want)
		}
	}
	merged := mergeHarnessEnvironment(
		[]string{
			"PATH=/bin",
			"GATEWAY_OPENSTACK_E2E=false",
			"GATEWAY_OPENSTACK_E2E_RUN_ID=foreign",
			"GATEWAY_OPENSTACK_E2E_AUDIT=false",
			"GATEWAY_OPENSTACK_API_QPS=foreign",
			"OS_AUTH_URL=https://foreign.example.test",
		},
		environment,
	)
	values := environmentMap(merged)
	if values["PATH"] != "/bin" || values["GATEWAY_OPENSTACK_E2E_RUN_ID"] != config.runID || values["GATEWAY_OPENSTACK_E2E_AUDIT"] != "true" {
		t.Fatalf("merged environment = %#v", values)
	}
	if _, found := values["GATEWAY_OPENSTACK_API_QPS"]; found {
		t.Fatal("merged environment retained an ambient controller setting")
	}
	if _, found := values["OS_AUTH_URL"]; found {
		t.Fatal("merged environment retained an ambient OpenStack credential setting")
	}
}

func TestPreflightKubernetesRejectsRunIdentityCollisions(t *testing.T) {
	config, err := resolveFileConfig(validFileConfig(), resolveOptions{repositoryRoot: testRepositoryRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		objects []client.Object
		wantErr bool
	}{
		{name: "empty cluster"},
		{
			name: "workload Namespace exists",
			objects: []client.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name: config.workloadNamespace,
			}}},
			wantErr: true,
		},
		{
			name: "controller Namespace exists",
			objects: []client.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name: config.controllerNamespace,
			}}},
			wantErr: true,
		},
		{
			name: "GatewayClass name exists",
			objects: []client.Object{&gatewayv1.GatewayClass{
				ObjectMeta: metav1.ObjectMeta{Name: config.gatewayClassName},
				Spec:       gatewayv1.GatewayClassSpec{ControllerName: "another.example.test/controller"},
			}},
			wantErr: true,
		},
		{
			name: "controller identity is selected",
			objects: []client.Object{&gatewayv1.GatewayClass{
				ObjectMeta: metav1.ObjectMeta{Name: "another-class"},
				Spec:       gatewayv1.GatewayClassSpec{ControllerName: gatewayv1.GatewayController(config.controllerName)},
			}},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := gatewayv1.Install(scheme); err != nil {
				t.Fatal(err)
			}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(test.objects...).Build()
			err := preflightKubernetes(context.Background(), kubeClient, config)
			if (err != nil) != test.wantErr {
				t.Fatalf("preflightKubernetes() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func TestPreflightOpenStackAuthenticatesBothCredentialsBeforeAudit(t *testing.T) {
	config, err := resolveFileConfig(validFileConfig(), resolveOptions{repositoryRoot: testRepositoryRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	controllerClouds := []byte(`clouds:
  openstack-e2e:
    auth_type: v3applicationcredential
    auth:
      auth_url: https://keystone.example.test/v3
      application_credential_id: controller-id
      application_credential_secret: controller-secret
    region_name: RegionOne
    verify: true
`)
	authenticatedPaths := make([]string, 0, 2)
	auditCalls := 0
	authenticate := func(_ context.Context, path string, cloud string, region string) (string, error) {
		if cloud != config.cloud || region != config.region {
			t.Fatalf("authenticate cloud = %q, region = %q", cloud, region)
		}
		authenticatedPaths = append(authenticatedPaths, path)
		if len(authenticatedPaths) == 1 {
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(contents, controllerClouds) || path == config.controllerCloudsYAML {
				t.Fatal("controller credential was not authenticated from an isolated exact copy")
			}
		}
		return config.expectedProjectID, nil
	}
	runAudit := func(_ context.Context, got resolvedConfig, environment []string) error {
		auditCalls++
		if got.runID != config.runID {
			t.Fatalf("audit config run ID = %q", got.runID)
		}
		for _, entry := range environment {
			if strings.HasPrefix(entry, "OS_") || strings.HasPrefix(entry, "GATEWAY_OPENSTACK_") {
				t.Fatalf("audit environment retained %q", entry)
			}
		}
		return nil
	}
	if err := preflightOpenStack(context.Background(), config, controllerClouds, authenticate, runAudit); err != nil {
		t.Fatalf("preflightOpenStack() error = %v", err)
	}
	if len(authenticatedPaths) != 2 || authenticatedPaths[1] != config.auditCloudsYAML || auditCalls != 1 {
		t.Fatalf("authenticated paths = %#v, audit calls = %d", authenticatedPaths, auditCalls)
	}

	auditCalls = 0
	authenticate = func(_ context.Context, path string, _ string, _ string) (string, error) {
		if path == config.auditCloudsYAML {
			return "another-project", nil
		}
		return config.expectedProjectID, nil
	}
	err = preflightOpenStack(context.Background(), config, controllerClouds, authenticate, runAudit)
	if err == nil || auditCalls != 0 || strings.Contains(err.Error(), config.expectedProjectID) || strings.Contains(err.Error(), "another-project") {
		t.Fatalf("preflightOpenStack() mismatch error = %v, audit calls = %d", err, auditCalls)
	}
}

func TestValidateControllerCloudsYAMLRejectsUnsafeOrUnsupportedInputs(t *testing.T) {
	valid := `clouds:
  openstack-e2e:
    auth_type: v3applicationcredential
    auth:
      auth_url: https://keystone.example.test/v3
      application_credential_id: controller-id
      application_credential_secret: sensitive-sentinel
    verify: true
`
	if err := validateControllerCloudsYAML([]byte(valid), "openstack-e2e"); err != nil {
		t.Fatalf("validateControllerCloudsYAML() error = %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(string) string
	}{
		{name: "TLS verification disabled", mutate: func(value string) string {
			return strings.Replace(value, "    verify: true\n", "    verify: false\n", 1)
		}},
		{name: "auxiliary CA", mutate: func(value string) string {
			return strings.Replace(value, "    verify: true\n", "    verify: true\n    cacert: /etc/openstack/ca.crt\n", 1)
		}},
		{name: "password auth", mutate: func(value string) string {
			return strings.Replace(value, "    auth_type: v3applicationcredential\n", "    auth_type: password\n", 1)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			contents := test.mutate(valid)
			err := validateControllerCloudsYAML([]byte(contents), "openstack-e2e")
			if err == nil || strings.Contains(err.Error(), "sensitive-sentinel") {
				t.Fatalf("validateControllerCloudsYAML() error = %v", err)
			}
		})
	}
}

func TestValidateEmptyOwnershipAudit(t *testing.T) {
	config, err := resolveFileConfig(validFileConfig(), resolveOptions{repositoryRoot: testRepositoryRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	report := audit.Report{
		FormatVersion: audit.ReportFormatVersion,
		Assessment:    audit.AssessmentComplete,
		Scope: audit.ReportScope{
			ControllerName: config.controllerName,
			ClusterID:      config.clusterID,
		},
	}
	if err := validateEmptyOwnershipAudit(report, config); err != nil {
		t.Fatalf("validateEmptyOwnershipAudit() error = %v", err)
	}
	report.Summary.OpenStackResources = 1
	if err := validateEmptyOwnershipAudit(report, config); err == nil {
		t.Fatal("validateEmptyOwnershipAudit() accepted a nonempty scope")
	}
}

func TestBoundedBufferCapsRetainedAuditOutput(t *testing.T) {
	buffer := boundedBuffer{limit: 4}
	contents := []byte("sensitive-output")
	if written, err := buffer.Write(contents); err != nil || written != len(contents) {
		t.Fatalf("boundedBuffer.Write() = %d, %v", written, err)
	}
	if !buffer.overflow || len(buffer.Bytes()) != 5 {
		t.Fatalf("bounded buffer overflow = %t, retained = %d", buffer.overflow, len(buffer.Bytes()))
	}
}

func TestInstallAndRemoveControllerPreservesSourceIdentityAndOrder(t *testing.T) {
	config, err := resolveFileConfig(validFileConfig(), resolveOptions{repositoryRoot: testRepositoryRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	base, err := loadBaseObjects(testRepositoryRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	objects, err := buildControllerObjects(base, config, []byte("clouds: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, rbacv1.AddToScheme} {
		if err := install(scheme); err != nil {
			t.Fatal(err)
		}
	}
	kubeClient := &recordingClient{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	installed, err := installController(context.Background(), kubeClient, objects, config)
	if err != nil {
		t.Fatalf("installController() error = %v", err)
	}
	wantCreates := []string{"Namespace", "ServiceAccount", "ClusterRole", "ClusterRoleBinding", "ConfigMap", "Secret", "Deployment"}
	if strings.Join(kubeClient.creates, ",") != strings.Join(wantCreates, ",") || !installed.deploymentAttempted {
		t.Fatalf("create order = %#v, deploymentAttempted = %t", kubeClient.creates, installed.deploymentAttempted)
	}
	var deployment appsv1.Deployment
	if err := kubeClient.Get(context.Background(), client.ObjectKey{Namespace: config.controllerNamespace, Name: controllerDeploymentName}, &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Template.Annotations[controllerConfigSourceAnnotation] == "" ||
		deployment.Spec.Template.Annotations[controllerCloudsSourceAnnotation] == "" {
		t.Fatal("Deployment was created without source UID and resourceVersion bindings")
	}
	if err := installed.remove(context.Background(), kubeClient); err != nil {
		t.Fatalf("installation.remove() error = %v", err)
	}
	wantDeletes := []string{"Deployment", "Secret", "ConfigMap", "ClusterRoleBinding", "ClusterRole", "ServiceAccount", "Namespace"}
	if strings.Join(kubeClient.deletes, ",") != strings.Join(wantDeletes, ",") {
		t.Fatalf("delete order = %#v", kubeClient.deletes)
	}
}

func TestPartialInstallCanBeRolledBackBeforeDeployment(t *testing.T) {
	config, err := resolveFileConfig(validFileConfig(), resolveOptions{repositoryRoot: testRepositoryRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	base, err := loadBaseObjects(testRepositoryRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	objects, err := buildControllerObjects(base, config, []byte("clouds: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, rbacv1.AddToScheme} {
		if err := install(scheme); err != nil {
			t.Fatal(err)
		}
	}
	kubeClient := &recordingClient{
		Client:         fake.NewClientBuilder().WithScheme(scheme).Build(),
		failCreateKind: "ConfigMap",
	}
	installed, err := installController(context.Background(), kubeClient, objects, config)
	if err == nil || installed.deploymentAttempted || len(installed.objects) == 0 {
		t.Fatalf("installController() installation = %#v, error = %v", installed, err)
	}
	if err := installed.remove(context.Background(), kubeClient); err != nil {
		t.Fatalf("remove partial installation: %v", err)
	}
	var namespace corev1.Namespace
	if err := kubeClient.Get(context.Background(), client.ObjectKey{Name: config.controllerNamespace}, &namespace); client.IgnoreNotFound(err) != nil || err == nil {
		t.Fatalf("partial Namespace remains after rollback: %v", err)
	}
}

func TestInstallationCleanupRefusesChangedIdentity(t *testing.T) {
	config, err := resolveFileConfig(validFileConfig(), resolveOptions{repositoryRoot: testRepositoryRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	base, err := loadBaseObjects(testRepositoryRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	objects, err := buildControllerObjects(base, config, []byte("clouds: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, rbacv1.AddToScheme} {
		if err := install(scheme); err != nil {
			t.Fatal(err)
		}
	}
	kubeClient := &recordingClient{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	installed, err := installController(context.Background(), kubeClient, objects, config)
	if err != nil {
		t.Fatal(err)
	}
	var deployment appsv1.Deployment
	key := client.ObjectKey{Namespace: config.controllerNamespace, Name: controllerDeploymentName}
	if err := kubeClient.Get(context.Background(), key, &deployment); err != nil {
		t.Fatal(err)
	}
	kubeClient.overrideRunAnnotation = "another-run"
	if err := installed.remove(context.Background(), kubeClient); err == nil {
		t.Fatal("installation.remove() accepted a changed run annotation")
	}
	if len(kubeClient.deletes) != 0 {
		t.Fatalf("cleanup deleted objects after identity mismatch: %#v", kubeClient.deletes)
	}
}

func validFileConfig() fileConfig {
	digest := "sha256:" + strings.Repeat("a", 64)
	return fileConfig{
		FormatVersion: configFormatVersion,
		RunID:         "run-20260901",
		Kubernetes: kubernetesConfig{
			DedicatedForE2E: true,
			Kubeconfig:      "/tmp/e2e.kubeconfig",
			Context:         "azimuth-e2e",
		},
		OpenStack: openStackConfig{
			ProjectMode:                     sharedProjectMode,
			AcceptProjectWideCredentialRisk: true,
			ExpectedProjectID:               "project-id",
			Cloud:                           "openstack-e2e",
			Region:                          "RegionOne",
			ControllerCloudsYAML:            "/tmp/controller-clouds.yaml",
			AuditCloudsYAML:                 "/tmp/audit-clouds.yaml",
			VIPSubnetID:                     "vip-subnet-id",
			MemberSubnetID:                  "member-subnet-id",
			ExternalNetworkID:               disabledNetwork,
		},
		Controller: controllerConfig{
			Domain:         "e2e.example.test",
			Image:          "registry.example.test/controller@" + digest,
			SourceRevision: strings.Repeat("b", 40),
		},
		Backend:   backendConfig{Image: "registry.example.test/agnhost:1.0@" + digest},
		Artifacts: artifactsConfig{Root: "/tmp/e2e-artifacts"},
	}
}

func testRepositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func environmentMap(environment []string) map[string]string {
	result := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, value, found := strings.Cut(entry, "=")
		if found {
			result[name] = value
		}
	}
	return result
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type recordingClient struct {
	client.Client
	creates               []string
	deletes               []string
	failCreateKind        string
	identities            map[string]types.UID
	overrideRunAnnotation string
}

func (c *recordingClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	kind := objectKind(object)
	if kind == c.failCreateKind {
		return errors.New("injected create failure")
	}
	if err := c.Client.Create(ctx, object, options...); err != nil {
		return err
	}
	if c.identities == nil {
		c.identities = make(map[string]types.UID)
	}
	uid := types.UID("uid-" + strings.ToLower(kind))
	c.identities[recordingObjectKey(object)] = uid
	object.SetUID(uid)
	if object.GetResourceVersion() == "" {
		object.SetResourceVersion("1")
	}
	c.creates = append(c.creates, kind)
	return nil
}

func (c *recordingClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, object, options...); err != nil {
		return err
	}
	if uid := c.identities[recordingObjectKey(object)]; uid != "" {
		object.SetUID(uid)
	}
	if object.GetResourceVersion() == "" {
		object.SetResourceVersion("1")
	}
	if c.overrideRunAnnotation != "" && objectKind(object) == "Deployment" {
		annotations := object.GetAnnotations()
		annotations[runAnnotation] = c.overrideRunAnnotation
		object.SetAnnotations(annotations)
	}
	return nil
}

func (c *recordingClient) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	if err := c.Client.Delete(ctx, object, options...); err != nil {
		return err
	}
	c.deletes = append(c.deletes, objectKind(object))
	return nil
}

func objectKind(object client.Object) string {
	switch object.(type) {
	case *corev1.Namespace:
		return "Namespace"
	case *corev1.ServiceAccount:
		return "ServiceAccount"
	case *corev1.ConfigMap:
		return "ConfigMap"
	case *corev1.Secret:
		return "Secret"
	case *appsv1.Deployment:
		return "Deployment"
	case *rbacv1.ClusterRole:
		return "ClusterRole"
	case *rbacv1.ClusterRoleBinding:
		return "ClusterRoleBinding"
	default:
		return "Unknown"
	}
}

func recordingObjectKey(object client.Object) string {
	return objectKind(object) + "/" + object.GetNamespace() + "/" + object.GetName()
}
