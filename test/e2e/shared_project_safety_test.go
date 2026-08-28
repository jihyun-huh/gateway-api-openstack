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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	testSharedProjectID = "project-for-shared-e2e"
	testSharedAppSecret = "application-credential-secret"
)

func TestVerifySharedProjectControllerAcceptsRunScopedConfiguration(t *testing.T) {
	suite, deployment := newSharedProjectSafetyFixture(t)
	if err := suite.verifySharedProjectController(context.Background(), deployment); err != nil {
		t.Fatalf("verifySharedProjectController() error = %v", err)
	}
}

func TestVerifySharedProjectControllerAllowsTheHarnessGatewayClassDuringRecovery(t *testing.T) {
	suite, deployment := newSharedProjectSafetyFixture(t)
	class := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testIdentityPrefix + suite.config.runID,
			UID:         types.UID("run-gateway-class-uid"),
			Annotations: map[string]string{dedicatedRunAnnotation: suite.config.runID},
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController(suite.config.controllerName),
		},
	}
	if err := suite.client.Create(context.Background(), class); err != nil {
		t.Fatal(err)
	}
	suite.createdClass = true
	suite.gatewayClassName = class.Name
	suite.gatewayClassUID = class.UID
	if err := suite.verifySharedProjectController(context.Background(), deployment); err != nil {
		t.Fatalf("verifySharedProjectController() error = %v", err)
	}
}

func TestSharedProjectRuntimeGuardsAreNoOpsForDedicatedMode(t *testing.T) {
	suite := phase2Suite{config: e2eConfig{project: projectConfig{mode: projectModeDedicated}}}
	if err := suite.verifySharedProjectController(context.Background(), nil); err != nil {
		t.Fatalf("verifySharedProjectController() error = %v", err)
	}
	if err := suite.verifyAuthenticatedSharedProject(context.Background()); err != nil {
		t.Fatalf("verifyAuthenticatedSharedProject() error = %v", err)
	}
}

func TestVerifySharedProjectControllerRejectsUnsafeConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*phase2Suite, *appsv1.Deployment)
	}{
		{
			name: "protected argument override",
			mutate: func(_ *phase2Suite, deployment *appsv1.Deployment) {
				deployment.Spec.Template.Spec.Containers[0].Args = append(
					deployment.Spec.Template.Spec.Containers[0].Args,
					"--vip-subnet-id=another-subnet",
				)
			},
		},
		{
			name: "direct environment override",
			mutate: func(_ *phase2Suite, deployment *appsv1.Deployment) {
				deployment.Spec.Template.Spec.Containers[0].Env = append(
					deployment.Spec.Template.Spec.Containers[0].Env,
					corev1.EnvVar{Name: "GATEWAY_OPENSTACK_CLUSTER_ID", Value: "another-cluster"},
				)
			},
		},
		{
			name: "container command override",
			mutate: func(_ *phase2Suite, deployment *appsv1.Deployment) {
				deployment.Spec.Template.Spec.Containers[0].Command = []string{"/bin/sh", "-c"}
			},
		},
		{
			name: "pod template is not bound to config source",
			mutate: func(_ *phase2Suite, deployment *appsv1.Deployment) {
				delete(deployment.Spec.Template.Annotations, controllerConfigSourceAnnotation)
			},
		},
		{
			name: "ConfigMap selects another provider",
			mutate: func(suite *phase2Suite, _ *appsv1.Deployment) {
				var configMap corev1.ConfigMap
				if err := suite.client.Get(context.Background(), types.NamespacedName{
					Namespace: suite.config.controllerNamespace,
					Name:      "controller-config",
				}, &configMap); err != nil {
					t.Fatal(err)
				}
				configMap.Data["GATEWAY_OPENSTACK_OCTAVIA_PROVIDER"] = "ovn"
				if err := suite.client.Update(context.Background(), &configMap); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "controller authenticates to another project",
			mutate: func(suite *phase2Suite, _ *appsv1.Deployment) {
				suite.authenticatedControllerProjectID = func(
					context.Context,
					*corev1.Secret,
					*corev1.SecretVolumeSource,
					string,
					string,
				) (string, error) {
					return "different-project", nil
				}
			},
		},
		{
			name: "controller identity already selected",
			mutate: func(suite *phase2Suite, _ *appsv1.Deployment) {
				class := &gatewayv1.GatewayClass{
					ObjectMeta: metav1.ObjectMeta{Name: "existing-class"},
					Spec: gatewayv1.GatewayClassSpec{
						ControllerName: gatewayv1.GatewayController(suite.config.controllerName),
					},
				}
				if err := suite.client.Create(context.Background(), class); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			suite, deployment := newSharedProjectSafetyFixture(t)
			test.mutate(suite, deployment)
			err := suite.verifySharedProjectController(context.Background(), deployment)
			if err == nil {
				t.Fatal("verifySharedProjectController() accepted unsafe configuration")
			}
			for _, secret := range []string{
				testSharedProjectID,
				testSharedAppSecret,
				suite.config.project.expectedVIPSubnetID,
				suite.config.project.expectedMemberSubnetID,
			} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error contains protected value %q: %v", secret, err)
				}
			}
		})
	}
}

func TestVerifyAuthenticatedSharedProjectRejectsMismatchWithoutIDs(t *testing.T) {
	config := sharedProjectTestConfig(t)
	suite := phase2Suite{
		config: config,
		authenticatedProjectID: func(context.Context, auditConfig) (string, error) {
			return "different-project", nil
		},
	}
	err := suite.verifyAuthenticatedSharedProject(context.Background())
	if err == nil {
		t.Fatal("verifyAuthenticatedSharedProject() accepted another project")
	}
	if strings.Contains(err.Error(), config.project.expectedProjectID) || strings.Contains(err.Error(), "different-project") {
		t.Fatalf("error exposed a project ID: %v", err)
	}
}

func TestVerifyAuthenticatedSharedProjectAcceptsExpectedProject(t *testing.T) {
	config := sharedProjectTestConfig(t)
	suite := phase2Suite{
		config: config,
		authenticatedProjectID: func(context.Context, auditConfig) (string, error) {
			return config.project.expectedProjectID, nil
		},
	}
	if err := suite.verifyAuthenticatedSharedProject(context.Background()); err != nil {
		t.Fatalf("verifyAuthenticatedSharedProject() error = %v", err)
	}
}

func TestMountsCloudsYAMLAllowsAdditionalTLSFiles(t *testing.T) {
	source := &corev1.SecretVolumeSource{Items: []corev1.KeyToPath{
		{Key: "clouds.yaml", Path: "clouds.yaml"},
		{Key: "ca.crt", Path: "ca.crt"},
	}}
	if !mountsCloudsYAML(source) {
		t.Fatal("mountsCloudsYAML() rejected an additional TLS file")
	}
}

func TestControllerAuthenticationOptionsRequireApplicationCredential(t *testing.T) {
	for _, test := range []struct {
		name     string
		contents []byte
	}{
		{name: "valid without project ID", contents: sharedCloudsYAML("", testSharedAppSecret)},
		{name: "password authentication", contents: []byte(`clouds:
  openstack-e2e:
    auth:
      auth_url: https://identity.example.test/v3
      username: e2e
      password: not-allowed
      project_id: project-for-shared-e2e
`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			secret := &corev1.Secret{Data: map[string][]byte{"clouds.yaml": test.contents}}
			source := &corev1.SecretVolumeSource{Items: []corev1.KeyToPath{{Key: "clouds.yaml", Path: "clouds.yaml"}}}
			_, _, cleanup, err := controllerAuthenticationOptions(secret, source, "openstack-e2e", "RegionOne")
			cleanup()
			if test.name == "valid without project ID" && err != nil {
				t.Fatalf("controllerAuthenticationOptions() error = %v", err)
			}
			if test.name != "valid without project ID" && err == nil {
				t.Fatal("controllerAuthenticationOptions() accepted an unsafe credential")
			}
		})
	}
}

func newSharedProjectSafetyFixture(t *testing.T) (*phase2Suite, *appsv1.Deployment) {
	t.Helper()
	config := sharedProjectTestConfig(t)
	immutable := true
	controllerNamespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        config.controllerNamespace,
			UID:         types.UID("controller-namespace-uid"),
			Annotations: map[string]string{dedicatedRunAnnotation: config.runID},
		},
	}
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       config.controllerNamespace,
			Name:            "controller-config",
			UID:             types.UID("controller-config-uid"),
			ResourceVersion: "17",
			Annotations:     map[string]string{dedicatedRunAnnotation: config.runID},
		},
		Immutable: &immutable,
		Data: map[string]string{
			"GATEWAY_OPENSTACK_CONTROLLER_NAME":     config.controllerName,
			"GATEWAY_OPENSTACK_CLUSTER_ID":          config.audit.clusterID,
			"GATEWAY_OPENSTACK_OCTAVIA_PROVIDER":    "amphora",
			"GATEWAY_OPENSTACK_VIP_SUBNET_ID":       config.project.expectedVIPSubnetID,
			"GATEWAY_OPENSTACK_EXTERNAL_NETWORK_ID": config.project.expectedExternalNetworkID,
			"GATEWAY_OPENSTACK_MEMBER_SUBNET_ID":    config.project.expectedMemberSubnetID,
			"GATEWAY_OPENSTACK_MEMBER_MODE":         "NodePort",
			"GATEWAY_OPENSTACK_NODE_ADDRESS_TYPE":   "InternalIP",
			"OS_CLIENT_CONFIG_FILE":                 controllerCloudsMountPath + "/clouds.yaml",
			"OS_CLOUD":                              config.audit.cloud,
			"OS_REGION_NAME":                        config.audit.region,
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       config.controllerNamespace,
			Name:            config.project.controllerCloudsSecret,
			UID:             types.UID("controller-secret-uid"),
			ResourceVersion: "23",
			Annotations:     map[string]string{dedicatedRunAnnotation: config.runID},
		},
		Immutable: &immutable,
		Data:      map[string][]byte{"clouds.yaml": sharedCloudsYAML("", testSharedAppSecret)},
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: config.controllerNamespace,
			Name:      config.controllerDeployment,
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
					controllerConfigSourceAnnotation:       string(configMap.UID) + "/" + configMap.ResourceVersion,
					controllerCloudsSecretSourceAnnotation: string(secret.UID) + "/" + secret.ResourceVersion,
				}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: config.controllerContainer,
						Args: []string{
							"--leader-elect=true",
							"--metrics-bind-address=:8080",
							"--octavia-microversion=" + config.audit.microversion,
						},
						EnvFrom: []corev1.EnvFromSource{{
							ConfigMapRef: &corev1.ConfigMapEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: configMap.Name},
							},
						}},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "openstack-clouds",
							MountPath: controllerCloudsMountPath,
							ReadOnly:  true,
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "openstack-clouds",
						VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: config.project.controllerCloudsSecret,
							Items:      []corev1.KeyToPath{{Key: "clouds.yaml", Path: "clouds.yaml"}},
						}},
					}},
				},
			},
		},
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	suite := &phase2Suite{
		config: config,
		client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			controllerNamespace,
			configMap,
			secret,
		).Build(),
		authenticatedControllerProjectID: func(
			context.Context,
			*corev1.Secret,
			*corev1.SecretVolumeSource,
			string,
			string,
		) (string, error) {
			return testSharedProjectID, nil
		},
	}
	return suite, deployment
}

func sharedProjectTestConfig(t *testing.T) e2eConfig {
	t.Helper()
	root := t.TempDir()
	cloudsPath := filepath.Join(root, "clouds.yaml")
	if err := os.WriteFile(cloudsPath, sharedCloudsYAML("", testSharedAppSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	return e2eConfig{
		runID:                "run-1234",
		controllerName:       "example.test/gao-e2e-run-1234",
		controllerNamespace:  "openstack-gateway-e2e-run-1234",
		controllerDeployment: "gateway-controller",
		controllerContainer:  "controller",
		project: projectConfig{
			mode:                      projectModeShared,
			expectedProjectID:         testSharedProjectID,
			expectedVIPSubnetID:       "vip-subnet-for-e2e",
			expectedMemberSubnetID:    "member-subnet-for-e2e",
			expectedExternalNetworkID: "",
			externalNetworkSet:        true,
			controllerCloudsSecret:    "openstack-clouds-run-1234",
		},
		audit: auditConfig{
			enabled:      true,
			clusterID:    "gao-e2e-run-1234",
			cloudsYAML:   cloudsPath,
			cloud:        "openstack-e2e",
			region:       "RegionOne",
			microversion: defaultAuditMicroversion,
		},
	}
}

func sharedCloudsYAML(projectID, secret string) []byte {
	projectLine := ""
	if projectID != "" {
		projectLine = "      project_id: " + projectID + "\n"
	}
	return []byte("clouds:\n" +
		"  openstack-e2e:\n" +
		"    auth_type: v3applicationcredential\n" +
		"    region_name: RegionOne\n" +
		"    auth:\n" +
		"      auth_url: https://identity.example.test/v3\n" +
		"      application_credential_id: app-credential-id\n" +
		"      application_credential_secret: " + secret + "\n" +
		projectLine)
}
