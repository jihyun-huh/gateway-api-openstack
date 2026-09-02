//go:build e2e

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
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/test/e2e/internal/cloudauth"
	"github.com/jihyun-huh/gateway-api-openstack/test/e2e/internal/controllercontract"
	"github.com/jihyun-huh/gateway-api-openstack/test/e2e/internal/runconfig"
)

const (
	testProjectID = "project-for-e2e"
	testAppSecret = "application-credential-secret"
)

func TestVerifyRunScopedControllerAcceptsBothProjectModes(t *testing.T) {
	for _, mode := range []projectMode{projectModeDedicated, projectModeShared} {
		t.Run(string(mode), func(t *testing.T) {
			suite, deployment := newRunScopedControllerFixture(t, mode)
			if err := suite.verifyRunScopedController(context.Background(), deployment); err != nil {
				t.Fatalf("verifyRunScopedController() error = %v", err)
			}
		})
	}
}

func TestVerifyRunScopedControllerAllowsHarnessGatewayClassDuringRecovery(t *testing.T) {
	suite, deployment := newRunScopedControllerFixture(t, projectModeDedicated)
	class := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:        suite.config.GatewayClassName,
			UID:         types.UID("run-gateway-class-uid"),
			Annotations: map[string]string{runAnnotation: suite.config.RunID},
		},
		Spec: gatewayv1.GatewayClassSpec{ControllerName: gatewayv1.GatewayController(suite.config.ControllerName)},
	}
	if err := suite.client.Create(context.Background(), class); err != nil {
		t.Fatal(err)
	}
	suite.createdClass = true
	suite.gatewayClassName = class.Name
	suite.gatewayClassUID = class.UID
	if err := suite.verifyRunScopedController(context.Background(), deployment); err != nil {
		t.Fatalf("verifyRunScopedController() error = %v", err)
	}
}

func TestVerifyRunScopedControllerRejectsUnsafeConfiguration(t *testing.T) {
	for _, test := range unsafeControllerMutations(t) {
		t.Run(test.name, func(t *testing.T) {
			suite, deployment := newRunScopedControllerFixture(t, projectModeShared)
			test.mutate(suite, deployment)
			err := suite.verifyRunScopedController(context.Background(), deployment)
			if err == nil {
				t.Fatal("verifyRunScopedController() accepted unsafe configuration")
			}
			assertControllerErrorRedacted(t, suite.config, err)
		})
	}
}

type unsafeControllerMutation struct {
	name   string
	mutate func(*phase2Suite, *appsv1.Deployment)
}

func unsafeControllerMutations(t *testing.T) []unsafeControllerMutation {
	t.Helper()
	return []unsafeControllerMutation{
		{name: "protected argument override", mutate: addProtectedControllerArgument},
		{name: "direct environment override", mutate: addDirectControllerEnvironment},
		{name: "container command override", mutate: overrideControllerCommand},
		{name: "pod template is not bound to config source", mutate: removeControllerConfigSource},
		{name: "ConfigMap selects another provider", mutate: setControllerConfigMapValue(t, controllercontract.EnvOctaviaProvider, "ovn")},
		{name: "ConfigMap contains an unexpected setting", mutate: setControllerConfigMapValue(t, "GATEWAY_OPENSTACK_UNREVIEWED", "unexpected")},
		{name: "controller authenticates to another project", mutate: resolveAnotherControllerProject},
		{name: "controller identity already selected", mutate: createControllerIdentityCollision(t)},
	}
}

func addProtectedControllerArgument(_ *phase2Suite, deployment *appsv1.Deployment) {
	deployment.Spec.Template.Spec.Containers[0].Args = append(
		deployment.Spec.Template.Spec.Containers[0].Args,
		"--vip-subnet-id=another-subnet",
	)
}

func addDirectControllerEnvironment(_ *phase2Suite, deployment *appsv1.Deployment) {
	deployment.Spec.Template.Spec.Containers[0].Env = append(
		deployment.Spec.Template.Spec.Containers[0].Env,
		corev1.EnvVar{Name: controllercontract.EnvClusterID, Value: "another-cluster"},
	)
}

func overrideControllerCommand(_ *phase2Suite, deployment *appsv1.Deployment) {
	deployment.Spec.Template.Spec.Containers[0].Command = []string{"/bin/sh", "-c"}
}

func removeControllerConfigSource(_ *phase2Suite, deployment *appsv1.Deployment) {
	delete(deployment.Spec.Template.Annotations, controllercontract.ConfigSourceAnnotation)
}

func setControllerConfigMapValue(t *testing.T, key, value string) func(*phase2Suite, *appsv1.Deployment) {
	t.Helper()
	return func(suite *phase2Suite, _ *appsv1.Deployment) {
		var configMap corev1.ConfigMap
		name := types.NamespacedName{Namespace: suite.config.ControllerNamespace, Name: "controller-config"}
		if err := suite.client.Get(context.Background(), name, &configMap); err != nil {
			t.Fatal(err)
		}
		configMap.Data[key] = value
		if err := suite.client.Update(context.Background(), &configMap); err != nil {
			t.Fatal(err)
		}
	}
}

func resolveAnotherControllerProject(suite *phase2Suite, _ *appsv1.Deployment) {
	suite.projectIDResolver = func(context.Context, string, string, string, string) (string, error) {
		return "different-project", nil
	}
}

func createControllerIdentityCollision(t *testing.T) func(*phase2Suite, *appsv1.Deployment) {
	t.Helper()
	return func(suite *phase2Suite, _ *appsv1.Deployment) {
		class := &gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "existing-class"},
			Spec:       gatewayv1.GatewayClassSpec{ControllerName: gatewayv1.GatewayController(suite.config.ControllerName)},
		}
		if err := suite.client.Create(context.Background(), class); err != nil {
			t.Fatal(err)
		}
	}
}

func assertControllerErrorRedacted(t *testing.T, config e2eConfig, err error) {
	t.Helper()
	protectedValues := []string{
		testProjectID,
		testAppSecret,
		config.Project.ExpectedVIPSubnetID,
		config.Project.ExpectedMemberSubnetID,
	}
	for _, protected := range protectedValues {
		if strings.Contains(err.Error(), protected) {
			t.Fatalf("error contains protected value %q: %v", protected, err)
		}
	}
}

func TestVerifyAuthenticatedAuditProjectAppliesToBothModes(t *testing.T) {
	for _, mode := range []projectMode{projectModeDedicated, projectModeShared} {
		t.Run(string(mode), func(t *testing.T) {
			config := runScopedControllerTestConfig(t, mode)
			suite := phase2Suite{
				config: config,
				projectIDResolver: func(context.Context, string, string, string, string) (string, error) {
					return config.Project.ExpectedProjectID, nil
				},
			}
			if err := suite.verifyAuthenticatedAuditProject(context.Background()); err != nil {
				t.Fatalf("verifyAuthenticatedAuditProject() error = %v", err)
			}
		})
	}
}

func TestVerifyAuthenticatedAuditProjectRejectsMismatchWithoutIDs(t *testing.T) {
	config := runScopedControllerTestConfig(t, projectModeDedicated)
	suite := phase2Suite{
		config: config,
		projectIDResolver: func(context.Context, string, string, string, string) (string, error) {
			return "different-project", nil
		},
	}
	err := suite.verifyAuthenticatedAuditProject(context.Background())
	if err == nil {
		t.Fatal("verifyAuthenticatedAuditProject() accepted another project")
	}
	if strings.Contains(err.Error(), config.Project.ExpectedProjectID) || strings.Contains(err.Error(), "different-project") {
		t.Fatalf("error exposed a project ID: %v", err)
	}
}

func TestWithAuthenticatedAuditCloudsYAMLUsesExactPrivateCopy(t *testing.T) {
	config := runScopedControllerTestConfig(t, projectModeShared)
	want, err := os.ReadFile(config.Audit.CloudsYAML)
	if err != nil {
		t.Fatal(err)
	}
	var authenticatedPath string
	var callbackPath string
	suite := phase2Suite{
		config:            config,
		projectIDResolver: mutatingExactAuditResolver(t, config, want, &authenticatedPath),
	}
	err = suite.withAuthenticatedAuditCloudsYAML(context.Background(), func(path string) error {
		callbackPath = path
		requireExactCloudsYAML(t, path, want)
		return nil
	})
	if err != nil {
		t.Fatalf("withAuthenticatedAuditCloudsYAML() error = %v", err)
	}
	if callbackPath == "" || callbackPath != authenticatedPath {
		t.Fatalf("callback path = %q, authenticated path = %q", callbackPath, authenticatedPath)
	}
	if _, err := os.Stat(callbackPath); !os.IsNotExist(err) {
		t.Fatalf("authenticated audit copy remains: %v", err)
	}
}

func mutatingExactAuditResolver(
	t *testing.T,
	config e2eConfig,
	want []byte,
	authenticatedPath *string,
) cloudauth.ProjectIDResolver {
	t.Helper()
	return func(_ context.Context, path, _, _, _ string) (string, error) {
		*authenticatedPath = path
		requireExactCloudsYAML(t, path, want)
		if err := os.WriteFile(config.Audit.CloudsYAML, []byte("changed after authentication\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return config.Project.ExpectedProjectID, nil
	}
}

func requireExactCloudsYAML(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("clouds.yaml bytes did not match the authenticated copy")
	}
}

func TestMountsOnlyCloudsYAMLRejectsAdditionalFiles(t *testing.T) {
	source := &corev1.SecretVolumeSource{Items: []corev1.KeyToPath{
		{Key: "clouds.yaml", Path: "clouds.yaml"},
		{Key: "ca.crt", Path: "ca.crt"},
	}}
	if mountsOnlyCloudsYAML(source) {
		t.Fatal("mountsOnlyCloudsYAML() accepted an auxiliary TLS file")
	}
}

func newRunScopedControllerFixture(t *testing.T, mode projectMode) (*phase2Suite, *appsv1.Deployment) {
	t.Helper()
	config := runScopedControllerTestConfig(t, mode)
	sources := newRunScopedControllerSources(config)
	deployment := newRunScopedControllerDeployment(t, config, sources)
	suite := &phase2Suite{
		config: config,
		client: fake.NewClientBuilder().WithScheme(runScopedControllerScheme(t)).WithObjects(
			sources.namespace,
			sources.configMap,
			sources.secret,
		).Build(),
		projectIDResolver: func(context.Context, string, string, string, string) (string, error) {
			return testProjectID, nil
		},
	}
	return suite, deployment
}

type runScopedControllerSources struct {
	namespace *corev1.Namespace
	configMap *corev1.ConfigMap
	secret    *corev1.Secret
}

func newRunScopedControllerSources(config e2eConfig) runScopedControllerSources {
	immutable := true
	controllerNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: config.ControllerNamespace, UID: types.UID("controller-namespace-uid"),
		Annotations: map[string]string{runAnnotation: config.RunID},
	}}
	settings := controllercontract.Settings{
		ControllerName:    config.ControllerName,
		ClusterID:         config.Audit.ClusterID,
		VIPSubnetID:       config.Project.ExpectedVIPSubnetID,
		ExternalNetworkID: config.Project.ExpectedExternalNetworkID,
		MemberSubnetID:    config.Project.ExpectedMemberSubnetID,
		Cloud:             config.Audit.Cloud,
		Region:            config.Audit.Region,
	}
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: config.ControllerNamespace, Name: "controller-config", UID: types.UID("controller-config-uid"),
			ResourceVersion: "17", Annotations: map[string]string{runAnnotation: config.RunID},
		},
		Immutable: &immutable,
		Data:      settings.Environment(),
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: config.ControllerNamespace, Name: config.Project.ControllerCloudsSecret, UID: types.UID("controller-secret-uid"),
			ResourceVersion: "23", Annotations: map[string]string{runAnnotation: config.RunID},
		},
		Immutable: &immutable,
		Data:      map[string][]byte{"clouds.yaml": testCloudsYAML(testAppSecret)},
	}
	return runScopedControllerSources{namespace: controllerNamespace, configMap: configMap, secret: secret}
}

func newRunScopedControllerDeployment(
	t *testing.T,
	config e2eConfig,
	sources runScopedControllerSources,
) *appsv1.Deployment {
	t.Helper()
	configSource := requireControllerSourceVersion(t, sources.configMap.UID, sources.configMap.ResourceVersion)
	cloudsSource := requireControllerSourceVersion(t, sources.secret.UID, sources.secret.ResourceVersion)
	arguments, err := controllercontract.RequiredArguments(config.Audit.Microversion)
	if err != nil {
		t.Fatal(err)
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: config.ControllerNamespace, Name: config.ControllerDeployment},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				controllercontract.ConfigSourceAnnotation: configSource,
				controllercontract.CloudsSourceAnnotation: cloudsSource,
			}},
			Spec: runScopedControllerPodSpec(config, sources.configMap.Name, arguments),
		}},
	}
}

func runScopedControllerPodSpec(config e2eConfig, configMapName string, arguments []string) corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: config.ControllerContainer,
			Args: arguments,
			EnvFrom: []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
			}}},
			VolumeMounts: []corev1.VolumeMount{{
				Name: "openstack-clouds", MountPath: controllercontract.CloudsMountPath, ReadOnly: true,
			}},
		}},
		Volumes: []corev1.Volume{{
			Name: "openstack-clouds",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: config.Project.ControllerCloudsSecret,
				Items:      []corev1.KeyToPath{{Key: "clouds.yaml", Path: "clouds.yaml"}},
			}},
		}},
	}
}

func requireControllerSourceVersion(t *testing.T, uid types.UID, resourceVersion string) string {
	t.Helper()
	source, err := controllercontract.SourceVersion(uid, resourceVersion)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func runScopedControllerScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, gatewayv1.Install} {
		if err := install(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func runScopedControllerTestConfig(t *testing.T, mode projectMode) e2eConfig {
	t.Helper()
	root := t.TempDir()
	cloudsPath := filepath.Join(root, "clouds.yaml")
	if err := os.WriteFile(cloudsPath, testCloudsYAML(testAppSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	return e2eConfig{
		FormatVersion:         runconfig.FormatVersion,
		RunID:                 "run-1234",
		ControllerName:        "example.test/gao-e2e-run-1234",
		ControllerNamespace:   "openstack-gateway-e2e-run-1234",
		ControllerDeployment:  "openstack-gateway-controller",
		ControllerContainer:   "controller",
		ControllerImage:       "registry.example.test/controller@sha256:" + strings.Repeat("a", 64),
		ControllerImageDigest: "sha256:" + strings.Repeat("a", 64),
		GatewayClassName:      "gao-e2e-run-1234",
		PollInterval:          time.Millisecond,
		Project: runconfig.Project{
			Mode:                      mode,
			ExpectedProjectID:         testProjectID,
			ExpectedVIPSubnetID:       "vip-subnet-for-e2e",
			ExpectedMemberSubnetID:    "member-subnet-for-e2e",
			ExpectedExternalNetworkID: "",
			ControllerCloudsSecret:    "openstack-clouds",
		},
		Audit: runconfig.Audit{
			ClusterID: "gao-e2e-run-1234", CloudsYAML: cloudsPath,
			Cloud: "openstack-e2e", Region: "RegionOne", Microversion: "2.5",
		},
	}
}

func testCloudsYAML(secret string) []byte {
	return []byte("clouds:\n" +
		"  openstack-e2e:\n" +
		"    auth_type: v3applicationcredential\n" +
		"    region_name: RegionOne\n" +
		"    auth:\n" +
		"      auth_url: https://identity.example.test/v3\n" +
		"      application_credential_id: app-credential-id\n" +
		"      application_credential_secret: " + secret + "\n")
}
