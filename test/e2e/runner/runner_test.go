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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/test/e2e/internal/controllercontract"
	"github.com/jihyun-huh/gateway-api-openstack/test/e2e/internal/runconfig"
)

func TestOpenStackExampleMatchesStrictRunnerSchema(t *testing.T) {
	repositoryRoot := testRepositoryRoot(t)
	config, err := loadFileConfig(filepath.Join(repositoryRoot, "test/e2e/openstack.example.yaml"))
	if err != nil {
		t.Fatalf("loadFileConfig(example) error = %v", err)
	}
	if _, err := resolveFileConfig(config, resolveOptions{
		repositoryRoot: repositoryRoot,
		random:         bytes.NewReader([]byte{1, 2, 3, 4}),
	}); err != nil {
		t.Fatalf("resolveFileConfig(example) error = %v", err)
	}
}

func TestBuildControllerObjectsUsesRunScopedIdentity(t *testing.T) {
	config, _, objects := builtControllerObjects(t)
	assertRunAnnotations(t, config.RunID, objects)
	assertRunScopedRBAC(t, config, objects)
	assertImmutableConfiguration(t, objects)
	assertControllerEnvironment(t, config, objects.configMap.Data)
}

func assertRunAnnotations(t *testing.T, runID string, objects controllerObjects) {
	t.Helper()
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
		if object.GetAnnotations()[controllercontract.RunAnnotation] != runID {
			t.Errorf("%s run annotation = %q", name, object.GetAnnotations()[controllercontract.RunAnnotation])
		}
	}
}

func assertRunScopedRBAC(t *testing.T, config resolvedConfig, objects controllerObjects) {
	t.Helper()
	if objects.namespace.Name != config.ControllerNamespace || objects.serviceAccount.Namespace != config.ControllerNamespace ||
		objects.clusterRole.Name != config.ClusterRoleName || objects.clusterRoleBinding.RoleRef.Name != config.ClusterRoleName ||
		len(objects.clusterRoleBinding.Subjects) != 1 || objects.clusterRoleBinding.Subjects[0].Namespace != config.ControllerNamespace {
		t.Fatalf("run-scoped RBAC objects = %#v", objects)
	}
}

func assertImmutableConfiguration(t *testing.T, objects controllerObjects) {
	t.Helper()
	if objects.configMap.Immutable == nil || !*objects.configMap.Immutable || objects.secret.Immutable == nil || !*objects.secret.Immutable {
		t.Fatal("ConfigMap or Secret is mutable")
	}
}

func assertControllerEnvironment(t *testing.T, config resolvedConfig, actual map[string]string) {
	t.Helper()
	wantEnvironment := controllercontract.Settings{
		ControllerName: config.ControllerName, ClusterID: config.Audit.ClusterID,
		VIPSubnetID: config.Project.ExpectedVIPSubnetID, MemberSubnetID: config.Project.ExpectedMemberSubnetID,
		Cloud: config.Audit.Cloud, Region: config.Audit.Region,
	}.Environment()
	for name, want := range wantEnvironment {
		if got := actual[name]; got != want {
			t.Errorf("ConfigMap %s = %q, want %q", name, got, want)
		}
	}
}

func TestBuildControllerObjectsUsesImmutableDeploymentContract(t *testing.T) {
	config, _, objects := builtControllerObjects(t)
	container := objects.deployment.Spec.Template.Spec.Containers[0]
	assertControllerContainer(t, config, objects, container)
	assertControllerCloudsMount(t, objects, container)
	assertControllerArgumentsAndMetrics(t, container)
	assertDeploymentSourceBindings(t, objects)
}

func assertControllerContainer(t *testing.T, config resolvedConfig, objects controllerObjects, container corev1.Container) {
	t.Helper()
	if container.Name != config.ControllerContainer || container.Image != config.ControllerImage || len(container.Command) != 0 || len(container.Env) != 0 ||
		len(container.EnvFrom) != 1 || container.EnvFrom[0].ConfigMapRef == nil || container.EnvFrom[0].ConfigMapRef.Name != objects.configMap.Name {
		t.Fatalf("controller container = %#v", container)
	}
}

func assertControllerCloudsMount(t *testing.T, objects controllerObjects, container corev1.Container) {
	t.Helper()
	if len(container.VolumeMounts) != 1 || container.VolumeMounts[0].MountPath != controllercontract.CloudsMountPath || !container.VolumeMounts[0].ReadOnly ||
		len(objects.deployment.Spec.Template.Spec.Volumes) != 1 || objects.deployment.Spec.Template.Spec.Volumes[0].Secret == nil ||
		objects.deployment.Spec.Template.Spec.Volumes[0].Secret.SecretName != objects.secret.Name {
		t.Fatalf("controller clouds mount = %#v, volumes = %#v", container.VolumeMounts, objects.deployment.Spec.Template.Spec.Volumes)
	}
}

func assertControllerArgumentsAndMetrics(t *testing.T, container corev1.Container) {
	t.Helper()
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
}

func assertDeploymentSourceBindings(t *testing.T, objects controllerObjects) {
	t.Helper()
	objects.configMap.UID = types.UID("config-uid")
	objects.configMap.ResourceVersion = "12"
	objects.secret.UID = types.UID("secret-uid")
	objects.secret.ResourceVersion = "13"
	if err := bindDeploymentSources(objects.deployment, objects.configMap, objects.secret); err != nil {
		t.Fatal(err)
	}
	if got := objects.deployment.Spec.Template.Annotations[controllercontract.ConfigSourceAnnotation]; got != "config-uid/12" {
		t.Fatalf("config source annotation = %q", got)
	}
	if got := objects.deployment.Spec.Template.Annotations[controllercontract.CloudsSourceAnnotation]; got != "secret-uid/13" {
		t.Fatalf("clouds source annotation = %q", got)
	}
}

func TestBuildControllerObjectsRejectsProtectedArgument(t *testing.T) {
	config, base, _ := builtControllerObjects(t)
	base.deployment.Spec.Template.Spec.Containers[0].Args = append(
		base.deployment.Spec.Template.Spec.Containers[0].Args,
		"--controller-name=foreign.example.test/controller",
	)
	if _, err := buildControllerObjects(base, config, []byte("clouds: {}\n")); err == nil || !strings.Contains(err.Error(), "protected setting") {
		t.Fatalf("buildControllerObjects() accepted a protected argument override: %v", err)
	}
}

func TestBuildControllerObjectsDropsUnexpectedBaseConfigMapData(t *testing.T) {
	config, base, _ := builtControllerObjects(t)
	base.configMap.Data["GATEWAY_OPENSTACK_UNREVIEWED"] = "unexpected"
	objects, err := buildControllerObjects(base, config, []byte("clouds: {}\n"))
	if err != nil {
		t.Fatalf("buildControllerObjects() error = %v", err)
	}
	if _, found := objects.configMap.Data["GATEWAY_OPENSTACK_UNREVIEWED"]; found {
		t.Fatal("run-scoped ConfigMap retained an unexpected base setting")
	}
}

func builtControllerObjects(t *testing.T) (resolvedConfig, baseObjects, controllerObjects) {
	t.Helper()
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
	return config, base, objects
}

func TestMergeHarnessEnvironmentDropsInheritedOverrides(t *testing.T) {
	environment := map[string]string{
		"GATEWAY_OPENSTACK_E2E":                "true",
		"GATEWAY_OPENSTACK_E2E_RUNTIME_CONFIG": "/private/runtime.json",
	}
	merged := mergeHarnessEnvironment(
		[]string{
			"PATH=/bin",
			"GATEWAY_OPENSTACK_E2E=false",
			"GATEWAY_OPENSTACK_E2E_RUNTIME_CONFIG=/foreign/runtime.json",
			"GATEWAY_OPENSTACK_API_QPS=foreign",
			"OS_AUTH_URL=https://foreign.example.test",
		},
		environment,
	)
	values := environmentMap(merged)
	if values["PATH"] != "/bin" || values["GATEWAY_OPENSTACK_E2E"] != "true" ||
		values["GATEWAY_OPENSTACK_E2E_RUNTIME_CONFIG"] != "/private/runtime.json" {
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
	for _, test := range preflightCollisionCases(config) {
		t.Run(test.name, func(t *testing.T) {
			kubeClient := fake.NewClientBuilder().WithScheme(runnerTestScheme(t)).WithObjects(test.objects...).Build()
			err := preflightKubernetes(context.Background(), kubeClient, config)
			if (err != nil) != test.wantErr {
				t.Fatalf("preflightKubernetes() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

type preflightCollisionCase struct {
	name    string
	objects []client.Object
	wantErr bool
}

func preflightCollisionCases(config resolvedConfig) []preflightCollisionCase {
	return []preflightCollisionCase{
		{name: "empty cluster"},
		{
			name: "workload Namespace exists",
			objects: []client.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name: config.Namespace,
			}}},
			wantErr: true,
		},
		{
			name: "controller Namespace exists",
			objects: []client.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name: config.ControllerNamespace,
			}}},
			wantErr: true,
		},
		{
			name:    "ClusterRole exists",
			objects: []client.Object{&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: config.ClusterRoleName}}},
			wantErr: true,
		},
		{
			name:    "ClusterRoleBinding exists",
			objects: []client.Object{&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: config.ClusterRoleBindingName}}},
			wantErr: true,
		},
		{
			name: "GatewayClass name exists",
			objects: []client.Object{&gatewayv1.GatewayClass{
				ObjectMeta: metav1.ObjectMeta{Name: config.GatewayClassName},
				Spec:       gatewayv1.GatewayClassSpec{ControllerName: "another.example.test/controller"},
			}},
			wantErr: true,
		},
		{
			name: "controller identity is selected",
			objects: []client.Object{&gatewayv1.GatewayClass{
				ObjectMeta: metav1.ObjectMeta{Name: "another-class"},
				Spec:       gatewayv1.GatewayClassSpec{ControllerName: gatewayv1.GatewayController(config.ControllerName)},
			}},
			wantErr: true,
		},
	}
}

func TestPreflightOpenStackAuthenticatesBothCredentialsBeforeAudit(t *testing.T) {
	fixture := newPreflightCredentialsFixture(t)
	if err := preflightOpenStack(
		context.Background(), fixture.config, fixture.controllerClouds, fixture.authenticate, fixture.runAudit,
	); err != nil {
		t.Fatalf("preflightOpenStack() error = %v", err)
	}
	fixture.assertSuccess(t)
}

func TestPreflightOpenStackRejectsAuditProjectMismatchWithoutRunningAudit(t *testing.T) {
	fixture := newPreflightCredentialsFixture(t)
	authenticationCalls := 0
	authenticate := func(context.Context, string, string, string, string) (string, error) {
		authenticationCalls++
		if authenticationCalls == 2 {
			return "another-project", nil
		}
		return fixture.config.Project.ExpectedProjectID, nil
	}
	err := preflightOpenStack(context.Background(), fixture.config, fixture.controllerClouds, authenticate, fixture.runAudit)
	if err == nil || fixture.auditCalls != 0 || strings.Contains(err.Error(), fixture.config.Project.ExpectedProjectID) ||
		strings.Contains(err.Error(), "another-project") {
		t.Fatalf("preflightOpenStack() mismatch error = %v, audit calls = %d", err, fixture.auditCalls)
	}
}

type preflightCredentialsFixture struct {
	config             resolvedConfig
	controllerClouds   []byte
	auditClouds        []byte
	authenticatedPaths []string
	auditCalls         int
	auditCallbackPath  string
}

func newPreflightCredentialsFixture(t *testing.T) *preflightCredentialsFixture {
	t.Helper()
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
	auditClouds := bytes.ReplaceAll(controllerClouds, []byte("controller-id"), []byte("audit-id"))
	credentialDirectory := t.TempDir()
	config.ControllerCloudsYAML = filepath.Join(credentialDirectory, "controller-clouds.yaml")
	config.Audit.CloudsYAML = filepath.Join(credentialDirectory, "audit-clouds.yaml")
	if err := os.WriteFile(config.ControllerCloudsYAML, controllerClouds, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.Audit.CloudsYAML, auditClouds, 0o600); err != nil {
		t.Fatal(err)
	}
	return &preflightCredentialsFixture{
		config: config, controllerClouds: controllerClouds, auditClouds: auditClouds,
		authenticatedPaths: make([]string, 0, 2),
	}
}

func (f *preflightCredentialsFixture) authenticate(
	_ context.Context,
	path string,
	cloud string,
	region string,
	microversion string,
) (string, error) {
	if err := f.validateAuthenticationOptions(cloud, region, microversion); err != nil {
		return "", err
	}
	f.authenticatedPaths = append(f.authenticatedPaths, path)
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if err := f.validateCredentialCopy(path, contents); err != nil {
		return "", err
	}
	if f.authenticatingAuditCredential() {
		return f.config.Project.ExpectedProjectID, os.WriteFile(
			f.config.Audit.CloudsYAML,
			[]byte("changed after authentication\n"),
			0o600,
		)
	}
	return f.config.Project.ExpectedProjectID, nil
}

func (f *preflightCredentialsFixture) validateAuthenticationOptions(cloud, region, microversion string) error {
	if cloud != f.config.Audit.Cloud || region != f.config.Audit.Region || microversion != f.config.Audit.Microversion {
		return errors.New("authentication options do not match the fixture")
	}
	return nil
}

func (f *preflightCredentialsFixture) validateCredentialCopy(path string, contents []byte) error {
	want := f.controllerClouds
	if f.authenticatingAuditCredential() {
		want = f.auditClouds
	}
	if !bytes.Equal(contents, want) {
		return errors.New("credential copy contents do not match the fixture")
	}
	if path == f.config.ControllerCloudsYAML || path == f.config.Audit.CloudsYAML {
		return errors.New("credential was not authenticated from an isolated exact copy")
	}
	return nil
}

func (f *preflightCredentialsFixture) authenticatingAuditCredential() bool {
	return len(f.authenticatedPaths) == 2
}

func (f *preflightCredentialsFixture) runAudit(_ context.Context, got resolvedConfig, environment []string) error {
	f.auditCalls++
	if got.RunID != f.config.RunID {
		return errors.New("audit received another run ID")
	}
	f.auditCallbackPath = got.Audit.CloudsYAML
	if len(f.authenticatedPaths) != 2 || f.auditCallbackPath != f.authenticatedPaths[1] ||
		f.auditCallbackPath == f.config.Audit.CloudsYAML {
		return errors.New("audit did not receive the authenticated credential path")
	}
	contents, err := os.ReadFile(f.auditCallbackPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(contents, f.auditClouds) {
		return errors.New("audit did not receive the authenticated credential bytes")
	}
	for _, entry := range environment {
		if strings.HasPrefix(entry, "OS_") || strings.HasPrefix(entry, "GATEWAY_OPENSTACK_") {
			return fmt.Errorf("audit environment retained %q", entry)
		}
	}
	return nil
}

func (f *preflightCredentialsFixture) assertSuccess(t *testing.T) {
	t.Helper()
	if len(f.authenticatedPaths) != 2 || f.auditCalls != 1 {
		t.Fatalf("authenticated paths = %#v, audit calls = %d", f.authenticatedPaths, f.auditCalls)
	}
	if _, err := os.Stat(f.auditCallbackPath); !os.IsNotExist(err) {
		t.Fatalf("authenticated audit copy remains: %v", err)
	}
}

func TestInstallAndRemoveControllerPreservesSourceIdentityAndOrder(t *testing.T) {
	config, objects, scheme := controllerInstallFixture(t)
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
	if err := kubeClient.Get(context.Background(), client.ObjectKey{Namespace: config.ControllerNamespace, Name: config.ControllerDeployment}, &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Template.Annotations[controllercontract.ConfigSourceAnnotation] == "" ||
		deployment.Spec.Template.Annotations[controllercontract.CloudsSourceAnnotation] == "" {
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

func TestPartialInstallFailsClosedWhenCreateOutcomeIsNotVisible(t *testing.T) {
	config, objects, scheme := controllerInstallFixture(t)
	kubeClient := &recordingClient{
		Client:         fake.NewClientBuilder().WithScheme(scheme).Build(),
		failCreateKind: "ConfigMap",
	}
	installed, err := installController(context.Background(), kubeClient, objects, config)
	if err == nil || installed.deploymentAttempted || len(installed.objects) == 0 || !installed.cleanupBlocked {
		t.Fatalf("installController() installation = %#v, error = %v", installed, err)
	}
	if err := installed.remove(context.Background(), kubeClient); err == nil {
		t.Fatal("remove partial installation accepted an ambiguous Create outcome")
	}
	var namespace corev1.Namespace
	if err := kubeClient.Get(context.Background(), client.ObjectKey{Name: config.ControllerNamespace}, &namespace); err != nil {
		t.Fatalf("retained partial Namespace Get() error = %v", err)
	}
	if len(kubeClient.deletes) != 0 {
		t.Fatalf("cleanup deleted objects after an ambiguous Create outcome: %#v", kubeClient.deletes)
	}
}

func TestPartialInstallFailsClosedWhenCreateOutcomeCannotBeRead(t *testing.T) {
	config, objects, scheme := controllerInstallFixture(t)
	kubeClient := &recordingClient{
		Client:         fake.NewClientBuilder().WithScheme(scheme).Build(),
		failCreateKind: "ConfigMap",
		failGetKind:    "ConfigMap",
	}
	installed, err := installController(context.Background(), kubeClient, objects, config)
	if err == nil || !installed.cleanupBlocked {
		t.Fatalf("installController() installation = %#v, error = %v", installed, err)
	}
	if err := installed.remove(context.Background(), kubeClient); err == nil {
		t.Fatal("remove partial installation accepted an unreadable Create outcome")
	}
	if len(kubeClient.deletes) != 0 {
		t.Fatalf("cleanup deleted objects after an unreadable Create outcome: %#v", kubeClient.deletes)
	}
}

func TestPartialInstallDoesNotAdoptAlreadyExistingObject(t *testing.T) {
	config, objects, scheme := controllerInstallFixture(t)
	existing := objects.clusterRole.DeepCopy()
	kubeClient := &recordingClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()}
	installed, err := installController(context.Background(), kubeClient, objects, config)
	if err == nil || !installed.cleanupBlocked {
		t.Fatalf("installController() installation = %#v, error = %v", installed, err)
	}
	if err := installed.remove(context.Background(), kubeClient); err == nil {
		t.Fatal("remove partial installation accepted an AlreadyExists collision")
	}
	if len(kubeClient.deletes) != 0 {
		t.Fatalf("cleanup deleted objects after an AlreadyExists collision: %#v", kubeClient.deletes)
	}
	var observed rbacv1.ClusterRole
	if err := kubeClient.Get(context.Background(), client.ObjectKey{Name: config.ClusterRoleName}, &observed); err != nil {
		t.Fatalf("pre-existing ClusterRole was removed: %v", err)
	}
}

func TestPartialInstallRollsBackObjectCreatedAfterLostResponse(t *testing.T) {
	config, objects, scheme := controllerInstallFixture(t)
	kubeClient := &recordingClient{
		Client:                     fake.NewClientBuilder().WithScheme(scheme).Build(),
		failCreateAfterPersistKind: "ConfigMap",
	}
	installed, err := installController(context.Background(), kubeClient, objects, config)
	if err == nil || installed.deploymentAttempted || installed.cleanupBlocked {
		t.Fatalf("installController() installation = %#v, error = %v", installed, err)
	}
	if len(installed.objects) != 5 || objectKind(installed.objects[len(installed.objects)-1].object) != "ConfigMap" {
		t.Fatalf("recorded objects after lost response = %#v", installed.objects)
	}
	if err := installed.remove(context.Background(), kubeClient); err != nil {
		t.Fatalf("remove recovered partial installation: %v", err)
	}
	var configMap corev1.ConfigMap
	key := client.ObjectKey{Namespace: config.ControllerNamespace, Name: objects.configMap.Name}
	if err := kubeClient.Get(context.Background(), key, &configMap); err == nil || client.IgnoreNotFound(err) != nil {
		t.Fatalf("ConfigMap from lost response remains after rollback: %v", err)
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
	key := client.ObjectKey{Namespace: config.ControllerNamespace, Name: config.ControllerDeployment}
	if err := kubeClient.Get(context.Background(), key, &deployment); err != nil {
		t.Fatal(err)
	}
	kubeClient.overrideRunAnnotation = "another-run"
	kubeClient.overrideRunAnnotationKind = "Deployment"
	if err := installed.remove(context.Background(), kubeClient); err == nil {
		t.Fatal("installation.remove() accepted a changed run annotation")
	}
	if len(kubeClient.deletes) != 0 {
		t.Fatalf("cleanup deleted objects after identity mismatch: %#v", kubeClient.deletes)
	}
}

func TestInstallationCleanupValidatesLaterObjectBeforeAnyDelete(t *testing.T) {
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
	// ConfigMap is late enough in installation order that the old reverse-order
	// loop deleted the Deployment and Secret before discovering this mismatch.
	kubeClient.overrideRunAnnotation = "another-run"
	kubeClient.overrideRunAnnotationKind = "ConfigMap"
	if err := installed.remove(context.Background(), kubeClient); err == nil {
		t.Fatal("installation.remove() accepted a later object identity mismatch")
	}
	if len(kubeClient.deletes) != 0 {
		t.Fatalf("cleanup partially deleted objects before later validation failed: %#v", kubeClient.deletes)
	}
	var deployment appsv1.Deployment
	key := client.ObjectKey{Namespace: config.ControllerNamespace, Name: config.ControllerDeployment}
	if err := kubeClient.Get(context.Background(), key, &deployment); err != nil {
		t.Fatalf("Deployment was removed before full cleanup validation: %v", err)
	}
}

func TestRunUsesTheSameLifecycleForBothProjectModes(t *testing.T) {
	for _, mode := range []runconfig.ProjectMode{runconfig.ProjectModeDedicated, runconfig.ProjectModeShared} {
		t.Run(string(mode), func(t *testing.T) {
			config, kubeClient, options := preparedRunFixture(t, mode)
			waitCalls := 0
			harnessCalls := 0
			options.waitController = func(context.Context, client.Client, resolvedConfig, types.UID) error {
				waitCalls++
				return nil
			}
			options.executeHarness = func(_ context.Context, got resolvedConfig, _ runOptions) error {
				harnessCalls++
				if got.Project.Mode != mode {
					t.Fatalf("harness project mode = %q, want %q", got.Project.Mode, mode)
				}
				return nil
			}
			if err := run(context.Background(), config, options); err != nil {
				t.Fatalf("run() error = %v", err)
			}
			if waitCalls != 1 || harnessCalls != 1 {
				t.Fatalf("wait calls = %d, harness calls = %d", waitCalls, harnessCalls)
			}
			var namespace corev1.Namespace
			err := kubeClient.Get(context.Background(), client.ObjectKey{Name: config.ControllerNamespace}, &namespace)
			if client.IgnoreNotFound(err) != nil {
				t.Fatalf("read controller Namespace after successful cleanup: %v", err)
			}
			if err == nil {
				t.Fatal("successful run retained the controller Namespace")
			}
		})
	}
}

func TestRunRetainsControllerAfterDeploymentAttempt(t *testing.T) {
	tests := []struct {
		name       string
		failCreate string
		waitErr    error
		harnessErr error
	}{
		{name: "Deployment create", failCreate: "Deployment"},
		{name: "readiness", waitErr: errors.New("injected readiness failure")},
		{name: "harness", harnessErr: errors.New("injected harness failure")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config, kubeClient, options := preparedRunFixture(t, runconfig.ProjectModeDedicated)
			kubeClient.failCreateKind = test.failCreate
			options.waitController = func(context.Context, client.Client, resolvedConfig, types.UID) error {
				return test.waitErr
			}
			options.executeHarness = func(context.Context, resolvedConfig, runOptions) error {
				return test.harnessErr
			}
			if err := run(context.Background(), config, options); err == nil {
				t.Fatal("run() succeeded after an injected failure")
			}
			if len(kubeClient.deletes) != 0 {
				t.Fatalf("run() removed recovery objects after Deployment attempt: %#v", kubeClient.deletes)
			}
			var namespace corev1.Namespace
			if err := kubeClient.Get(context.Background(), client.ObjectKey{Name: config.ControllerNamespace}, &namespace); err != nil {
				t.Fatalf("retained controller Namespace Get() error = %v", err)
			}
		})
	}
}

func TestRunRollsBackFailureBeforeDeploymentAttempt(t *testing.T) {
	config, kubeClient, options := preparedRunFixture(t, runconfig.ProjectModeShared)
	kubeClient.failCreateAfterPersistKind = "ConfigMap"
	if err := run(context.Background(), config, options); err == nil || !strings.Contains(err.Error(), "removed the partial installation") {
		t.Fatalf("run() error = %v", err)
	}
	var namespace corev1.Namespace
	err := kubeClient.Get(context.Background(), client.ObjectKey{Name: config.ControllerNamespace}, &namespace)
	if err == nil || client.IgnoreNotFound(err) != nil {
		t.Fatalf("partial controller Namespace remains: %v", err)
	}
}

func TestRunRetainsObjectsWhenPreDeploymentCreateOutcomeIsAmbiguous(t *testing.T) {
	config, kubeClient, options := preparedRunFixture(t, runconfig.ProjectModeShared)
	kubeClient.failCreateKind = "ConfigMap"
	err := run(context.Background(), config, options)
	if err == nil || !strings.Contains(err.Error(), "requires review") || strings.Contains(err.Error(), "removed the partial installation") {
		t.Fatalf("run() error = %v", err)
	}
	if len(kubeClient.deletes) != 0 {
		t.Fatalf("run() deleted objects after an ambiguous Create outcome: %#v", kubeClient.deletes)
	}
	var namespace corev1.Namespace
	if err := kubeClient.Get(context.Background(), client.ObjectKey{Name: config.ControllerNamespace}, &namespace); err != nil {
		t.Fatalf("retained controller Namespace Get() error = %v", err)
	}
}

func preparedRunFixture(t *testing.T, mode runconfig.ProjectMode) (resolvedConfig, *recordingClient, runOptions) {
	t.Helper()
	root := testRepositoryRoot(t)
	directory := t.TempDir()
	file := preparedRunFile(t, mode, directory)
	config, err := resolveFileConfig(file, resolveOptions{repositoryRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	auditBinary := filepath.Join(directory, "openstack-gateway-audit")
	if err := os.WriteFile(auditBinary, []byte("synthetic executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	kubeClient := &recordingClient{Client: fake.NewClientBuilder().WithScheme(runnerTestScheme(t)).Build()}
	return config, kubeClient, preparedRunOptions(root, auditBinary, config, kubeClient)
}

func preparedRunFile(t *testing.T, mode runconfig.ProjectMode, directory string) fileConfig {
	t.Helper()
	file := validFileConfig()
	file.OpenStack.ProjectMode = mode
	file.OpenStack.DedicatedForE2E = mode == runconfig.ProjectModeDedicated
	file.OpenStack.AcceptProjectWideCredentialRisk = mode == runconfig.ProjectModeShared
	file.Kubernetes.Kubeconfig = filepath.Join(directory, "kubeconfig")
	file.OpenStack.ControllerCloudsYAML = filepath.Join(directory, "controller-clouds.yaml")
	file.OpenStack.AuditCloudsYAML = filepath.Join(directory, "audit-clouds.yaml")
	file.Artifacts.Root = filepath.Join(directory, "artifacts")
	writePreparedRunFiles(t, file)
	return file
}

func writePreparedRunFiles(t *testing.T, file fileConfig) {
	t.Helper()
	credential := []byte(`clouds:
  openstack-e2e:
    auth_type: v3applicationcredential
    auth:
      auth_url: https://keystone.example.test/v3
      application_credential_id: e2e-id
      application_credential_secret: e2e-secret
    region_name: RegionOne
    verify: true
`)
	for path, contents := range map[string][]byte{
		file.Kubernetes.Kubeconfig:          []byte("synthetic kubeconfig"),
		file.OpenStack.ControllerCloudsYAML: credential,
		file.OpenStack.AuditCloudsYAML:      credential,
	} {
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func runnerTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		rbacv1.AddToScheme,
		gatewayv1.Install,
	} {
		if err := install(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func preparedRunOptions(root, auditBinary string, config resolvedConfig, kubeClient client.Client) runOptions {
	return runOptions{
		repositoryRoot: root,
		auditBinary:    auditBinary,
		kubeClient:     kubeClient,
		authenticateProject: func(context.Context, string, string, string, string) (string, error) {
			return config.Project.ExpectedProjectID, nil
		},
		runAudit:       func(context.Context, resolvedConfig, []string) error { return nil },
		waitController: func(context.Context, client.Client, resolvedConfig, types.UID) error { return nil },
		executeHarness: func(context.Context, resolvedConfig, runOptions) error { return nil },
	}
}

func validFileConfig() fileConfig {
	digest := "sha256:" + strings.Repeat("a", 64)
	return fileConfig{
		FormatVersion: runconfig.FormatVersion,
		RunID:         "run-20260901",
		Kubernetes: runconfig.Kubernetes{
			DedicatedForE2E: true,
			Kubeconfig:      "/tmp/e2e.kubeconfig",
			Context:         "azimuth-e2e",
		},
		OpenStack: runconfig.OpenStack{
			ProjectMode:                     runconfig.ProjectModeShared,
			AcceptProjectWideCredentialRisk: true,
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
		Backend:   runconfig.Backend{Image: "registry.example.test/agnhost:1.0@" + digest},
		Artifacts: runconfig.Artifacts{Root: "/tmp/e2e-artifacts"},
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
	creates                    []string
	deletes                    []string
	failCreateKind             string
	failCreateAfterPersistKind string
	failGetKind                string
	identities                 map[string]types.UID
	overrideRunAnnotation      string
	overrideRunAnnotationKind  string
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
	if kind == c.failCreateAfterPersistKind {
		return errors.New("injected lost create response")
	}
	return nil
}

func (c *recordingClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if objectKind(object) == c.failGetKind {
		return errors.New("injected get failure")
	}
	if err := c.Client.Get(ctx, key, object, options...); err != nil {
		return err
	}
	if uid := c.identities[recordingObjectKey(object)]; uid != "" {
		object.SetUID(uid)
	}
	if object.GetResourceVersion() == "" {
		object.SetResourceVersion("1")
	}
	if c.overrideRunAnnotation != "" && objectKind(object) == c.overrideRunAnnotationKind {
		annotations := object.GetAnnotations()
		annotations[controllercontract.RunAnnotation] = c.overrideRunAnnotation
		object.SetAnnotations(annotations)
	}
	return nil
}

func controllerInstallFixture(t *testing.T) (resolvedConfig, controllerObjects, *runtime.Scheme) {
	t.Helper()
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
	return config, objects, scheme
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
