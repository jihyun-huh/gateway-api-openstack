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
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	gopherconfig "github.com/gophercloud/gophercloud/v2/openstack/config"
	openstackclouds "github.com/gophercloud/gophercloud/v2/openstack/config/clouds"
	tokensv2 "github.com/gophercloud/gophercloud/v2/openstack/identity/v2/tokens"
	tokensv3 "github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack"
)

const (
	controllerCloudsMountPath              = "/etc/openstack"
	controllerConfigSourceAnnotation       = "e2e.gateway-api-openstack.io/controller-config-source"
	controllerCloudsSecretSourceAnnotation = "e2e.gateway-api-openstack.io/controller-clouds-source"
)

type authenticatedProjectResolver func(context.Context, auditConfig) (string, error)
type controllerProjectResolver func(context.Context, *corev1.Secret, *corev1.SecretVolumeSource, string, string) (string, error)

var protectedControllerEnvironment = map[string]string{
	"GATEWAY_OPENSTACK_CONTROLLER_NAME":     "controller identity",
	"GATEWAY_OPENSTACK_CLUSTER_ID":          "cluster identity",
	"GATEWAY_OPENSTACK_OCTAVIA_PROVIDER":    "Octavia provider",
	"GATEWAY_OPENSTACK_VIP_SUBNET_ID":       "VIP subnet",
	"GATEWAY_OPENSTACK_EXTERNAL_NETWORK_ID": "external network",
	"GATEWAY_OPENSTACK_MEMBER_SUBNET_ID":    "member subnet",
	"GATEWAY_OPENSTACK_MEMBER_MODE":         "member mode",
	"GATEWAY_OPENSTACK_NODE_ADDRESS_TYPE":   "node address type",
	"OS_CLIENT_CONFIG_FILE":                 "clouds.yaml path",
	"OS_CLOUD":                              "OpenStack cloud",
	"OS_REGION_NAME":                        "OpenStack region",
}

var protectedControllerFlags = map[string]struct{}{
	"clouds-yaml":         {},
	"cluster-id":          {},
	"controller-name":     {},
	"external-network-id": {},
	"insecure":            {},
	"member-mode":         {},
	"member-subnet-id":    {},
	"node-address-type":   {},
	"octavia-provider":    {},
	"openstack-cloud":     {},
	"openstack-region":    {},
	"vip-subnet-id":       {},
}

func (s *phase2Suite) verifySharedProjectController(ctx context.Context, deployment *appsv1.Deployment) error {
	if s.config.project.mode != projectModeShared {
		return nil
	}

	var namespace corev1.Namespace
	if err := s.client.Get(ctx, client.ObjectKey{Name: s.config.controllerNamespace}, &namespace); err != nil {
		return fmt.Errorf("read run-scoped controller Namespace: %w", err)
	}
	if namespace.UID == "" || namespace.Annotations[dedicatedRunAnnotation] != s.config.runID {
		return fmt.Errorf("controller Namespace lacks the exact run annotation and immutable identity")
	}

	var classes gatewayv1.GatewayClassList
	if err := s.client.List(ctx, &classes); err != nil {
		return fmt.Errorf("list GatewayClasses before shared-project execution: %w", err)
	}
	for index := range classes.Items {
		class := &classes.Items[index]
		if string(class.Spec.ControllerName) != s.config.controllerName {
			continue
		}
		if s.createdClass && class.Name == s.gatewayClassName && class.UID == s.gatewayClassUID &&
			class.Annotations[dedicatedRunAnnotation] == s.config.runID {
			continue
		}
		return fmt.Errorf("the shared-project controller identity is already selected by a GatewayClass")
	}

	container, found := findContainer(deployment.Spec.Template.Spec.Containers, s.config.controllerContainer)
	if !found {
		return fmt.Errorf("configured controller container is absent")
	}
	configMap, err := s.sharedControllerConfigMap(ctx, &container)
	if err != nil {
		return err
	}
	secret, volume, err := s.sharedControllerCloudsSecret(ctx, deployment, &container)
	if err != nil {
		return err
	}
	if err := validateControllerSourceVersions(deployment, configMap, secret); err != nil {
		return err
	}
	if s.authenticatedControllerProjectID == nil {
		return fmt.Errorf("shared-project controller authentication verifier is unavailable")
	}
	projectID, err := s.authenticatedControllerProjectID(ctx, secret, volume, configMap.Data["OS_CLOUD"], s.config.audit.region)
	if err != nil || projectID == "" || projectID != s.config.project.expectedProjectID {
		return fmt.Errorf("controller credential did not authenticate to the approved shared-project scope")
	}
	return nil
}

func (s *phase2Suite) sharedControllerConfigMap(ctx context.Context, container *corev1.Container) (*corev1.ConfigMap, error) {
	if len(container.Command) != 0 {
		return nil, fmt.Errorf("shared-project controller must use the image entrypoint directly")
	}
	if len(container.EnvFrom) != 1 || container.EnvFrom[0].Prefix != "" || container.EnvFrom[0].ConfigMapRef == nil ||
		container.EnvFrom[0].SecretRef != nil ||
		container.EnvFrom[0].ConfigMapRef.Optional != nil && *container.EnvFrom[0].ConfigMapRef.Optional {
		return nil, fmt.Errorf("shared-project controller must use one unprefixed ConfigMap environment source")
	}
	for _, variable := range container.Env {
		if _, protected := protectedControllerEnvironment[variable.Name]; protected {
			return nil, fmt.Errorf("shared-project controller has a direct environment override for a protected setting")
		}
	}
	if err := validateSharedControllerArguments(container.Args, s.config.audit.microversion); err != nil {
		return nil, err
	}

	var configMap corev1.ConfigMap
	key := client.ObjectKey{Namespace: s.config.controllerNamespace, Name: container.EnvFrom[0].ConfigMapRef.Name}
	if err := s.client.Get(ctx, key, &configMap); err != nil {
		return nil, fmt.Errorf("read shared-project controller ConfigMap: %w", err)
	}
	if configMap.UID == "" || configMap.Annotations[dedicatedRunAnnotation] != s.config.runID || configMap.Immutable == nil || !*configMap.Immutable {
		return nil, fmt.Errorf("controller ConfigMap must be immutable and carry the exact run annotation")
	}
	expected := map[string]string{
		"GATEWAY_OPENSTACK_CONTROLLER_NAME":     s.config.controllerName,
		"GATEWAY_OPENSTACK_CLUSTER_ID":          s.config.audit.clusterID,
		"GATEWAY_OPENSTACK_OCTAVIA_PROVIDER":    "amphora",
		"GATEWAY_OPENSTACK_VIP_SUBNET_ID":       s.config.project.expectedVIPSubnetID,
		"GATEWAY_OPENSTACK_EXTERNAL_NETWORK_ID": s.config.project.expectedExternalNetworkID,
		"GATEWAY_OPENSTACK_MEMBER_SUBNET_ID":    s.config.project.expectedMemberSubnetID,
		"GATEWAY_OPENSTACK_MEMBER_MODE":         "NodePort",
		"GATEWAY_OPENSTACK_NODE_ADDRESS_TYPE":   "InternalIP",
		"OS_CLIENT_CONFIG_FILE":                 controllerCloudsMountPath + "/clouds.yaml",
		"OS_CLOUD":                              s.config.audit.cloud,
		"OS_REGION_NAME":                        s.config.audit.region,
	}
	for name, value := range expected {
		actual, found := configMap.Data[name]
		if !found || actual != value {
			return nil, fmt.Errorf("controller ConfigMap does not match the approved shared-project settings")
		}
	}
	return &configMap, nil
}

func validateControllerSourceVersions(deployment *appsv1.Deployment, configMap *corev1.ConfigMap, secret *corev1.Secret) error {
	if deployment == nil || configMap == nil || secret == nil || configMap.UID == "" || configMap.ResourceVersion == "" ||
		secret.UID == "" || secret.ResourceVersion == "" {
		return fmt.Errorf("controller configuration sources lack immutable API identity")
	}
	expectedConfig := string(configMap.UID) + "/" + configMap.ResourceVersion
	expectedSecret := string(secret.UID) + "/" + secret.ResourceVersion
	if deployment.Spec.Template.Annotations[controllerConfigSourceAnnotation] != expectedConfig ||
		deployment.Spec.Template.Annotations[controllerCloudsSecretSourceAnnotation] != expectedSecret {
		return fmt.Errorf("controller Pod template is not bound to the validated ConfigMap and Secret versions")
	}
	return nil
}

func validateSharedControllerArguments(arguments []string, microversion string) error {
	microversionArguments := 0
	for _, argument := range arguments {
		if !strings.HasPrefix(argument, "-") {
			continue
		}
		name := strings.TrimLeft(strings.SplitN(argument, "=", 2)[0], "-")
		if name == "octavia-microversion" {
			microversionArguments++
			if argument != "--octavia-microversion="+microversion {
				return fmt.Errorf("controller Deployment has an unexpected Octavia microversion argument")
			}
			continue
		}
		if _, protected := protectedControllerFlags[name]; protected {
			return fmt.Errorf("controller Deployment overrides a protected shared-project setting")
		}
	}
	if microversionArguments != 1 {
		return fmt.Errorf("controller Deployment must set the audited Octavia microversion exactly once")
	}
	return nil
}

func (s *phase2Suite) sharedControllerCloudsSecret(
	ctx context.Context,
	deployment *appsv1.Deployment,
	container *corev1.Container,
) (*corev1.Secret, *corev1.SecretVolumeSource, error) {
	volumeName := ""
	mounts := 0
	for _, mount := range container.VolumeMounts {
		if mount.MountPath != controllerCloudsMountPath {
			continue
		}
		mounts++
		if !mount.ReadOnly || mount.SubPath != "" || mount.SubPathExpr != "" {
			return nil, nil, fmt.Errorf("controller clouds.yaml mount is not an exact read-only directory mount")
		}
		volumeName = mount.Name
	}
	if mounts != 1 {
		return nil, nil, fmt.Errorf("controller Deployment must mount one clouds.yaml directory")
	}
	var volume *corev1.Volume
	for index := range deployment.Spec.Template.Spec.Volumes {
		if deployment.Spec.Template.Spec.Volumes[index].Name == volumeName {
			volume = &deployment.Spec.Template.Spec.Volumes[index]
			break
		}
	}
	if volume == nil || volume.Secret == nil || volume.Secret.SecretName != s.config.project.controllerCloudsSecret ||
		volume.Secret.Optional != nil && *volume.Secret.Optional || !mountsCloudsYAML(volume.Secret) {
		return nil, nil, fmt.Errorf("controller Deployment does not mount the approved clouds.yaml Secret")
	}

	var secret corev1.Secret
	key := client.ObjectKey{Namespace: s.config.controllerNamespace, Name: s.config.project.controllerCloudsSecret}
	if err := s.client.Get(ctx, key, &secret); err != nil {
		return nil, nil, fmt.Errorf("read shared-project controller Secret: %w", err)
	}
	if secret.UID == "" || secret.Annotations[dedicatedRunAnnotation] != s.config.runID || secret.Immutable == nil || !*secret.Immutable {
		return nil, nil, fmt.Errorf("controller Secret must be immutable and carry the exact run annotation")
	}
	if len(secret.Data["clouds.yaml"]) == 0 {
		return nil, nil, fmt.Errorf("controller Secret does not contain clouds.yaml")
	}
	return &secret, volume.Secret, nil
}

func mountsCloudsYAML(source *corev1.SecretVolumeSource) bool {
	if source == nil || len(source.Items) == 0 {
		return source != nil
	}
	matches := 0
	for _, item := range source.Items {
		if item.Key == "clouds.yaml" && filepath.Clean(item.Path) == "clouds.yaml" {
			matches++
		}
	}
	return matches == 1
}

func resolveAuthenticatedControllerProjectID(
	ctx context.Context,
	secret *corev1.Secret,
	source *corev1.SecretVolumeSource,
	cloudName string,
	region string,
) (string, error) {
	auth, tlsConfig, cleanup, err := controllerAuthenticationOptions(secret, source, cloudName, region)
	if err != nil {
		return "", err
	}
	defer cleanup()

	provider, err := gopherconfig.NewProviderClient(ctx, auth, gopherconfig.WithTLSConfig(tlsConfig))
	if err != nil {
		return "", fmt.Errorf("authenticate the controller credential")
	}
	return authenticatedProjectIDFromProvider(provider)
}

func controllerAuthenticationOptions(
	secret *corev1.Secret,
	source *corev1.SecretVolumeSource,
	cloudName string,
	region string,
) (gophercloud.AuthOptions, *tls.Config, func(), error) {
	if secret == nil || source == nil {
		return gophercloud.AuthOptions{}, nil, func() {}, fmt.Errorf("controller credential source is incomplete")
	}
	contents := secret.Data["clouds.yaml"]
	var document openstackclouds.Clouds
	if err := yaml.Unmarshal(contents, &document); err != nil {
		return gophercloud.AuthOptions{}, nil, func() {}, fmt.Errorf("controller clouds.yaml could not be parsed")
	}
	cloud, found := document.Clouds[cloudName]
	if !found || cloud.AuthInfo == nil || cloud.Profile != "" || cloud.Cloud != "" || cloud.Verify != nil && !*cloud.Verify {
		return gophercloud.AuthOptions{}, nil, func() {}, fmt.Errorf("controller clouds.yaml is not a self-contained verified cloud entry")
	}
	if cloud.AuthType != "" && cloud.AuthType != openstackclouds.AuthV3ApplicationCredential {
		return gophercloud.AuthOptions{}, nil, func() {}, fmt.Errorf("shared-project controller must use an application credential")
	}
	if err := validateControllerApplicationCredential(*cloud.AuthInfo); err != nil {
		return gophercloud.AuthOptions{}, nil, func() {}, err
	}

	tlsOptions, cleanup, err := controllerTLSParseOptions(secret, source, cloud)
	if err != nil {
		return gophercloud.AuthOptions{}, nil, func() {}, err
	}
	options := []openstackclouds.ParseOption{
		openstackclouds.WithCloudName(cloudName),
		openstackclouds.WithCloudsYAML(bytes.NewReader(contents)),
		openstackclouds.WithRegion(region),
	}
	options = append(options, tlsOptions...)
	auth, endpoint, tlsConfig, err := openstackclouds.Parse(options...)
	if err != nil {
		cleanup()
		return gophercloud.AuthOptions{}, nil, func() {}, fmt.Errorf("controller clouds.yaml could not be parsed")
	}
	if endpoint.Region != region || tlsConfig != nil && tlsConfig.InsecureSkipVerify {
		cleanup()
		return gophercloud.AuthOptions{}, nil, func() {}, fmt.Errorf("controller cloud does not use the approved region and TLS verification")
	}
	return auth, tlsConfig, cleanup, nil
}

func validateControllerApplicationCredential(auth openstackclouds.AuthInfo) error {
	identities := 0
	if auth.ApplicationCredentialID != "" {
		identities++
	}
	if auth.ApplicationCredentialName != "" {
		identities++
	}
	if auth.AuthURL == "" || identities != 1 || auth.ApplicationCredentialSecret == "" ||
		auth.Password != "" || auth.Token != "" || auth.SystemScope != "" || auth.TrustID != "" {
		return fmt.Errorf("controller credential does not satisfy the shared-project application credential contract")
	}
	if auth.ApplicationCredentialName != "" && auth.UserID == "" && auth.Username == "" {
		return fmt.Errorf("named application credential does not identify its user")
	}
	return nil
}

func controllerTLSParseOptions(
	secret *corev1.Secret,
	source *corev1.SecretVolumeSource,
	cloud openstackclouds.Cloud,
) ([]openstackclouds.ParseOption, func(), error) {
	directory, err := os.MkdirTemp("", "gateway-api-openstack-e2e-tls-")
	if err != nil {
		return nil, func() {}, fmt.Errorf("prepare controller TLS material")
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	paths := []struct {
		mounted string
		apply   func(string) openstackclouds.ParseOption
	}{
		{mounted: cloud.CACertFile, apply: openstackclouds.WithCACertPath},
		{mounted: cloud.ClientCertFile, apply: openstackclouds.WithClientCertPath},
		{mounted: cloud.ClientKeyFile, apply: openstackclouds.WithClientKeyPath},
	}
	options := make([]openstackclouds.ParseOption, 0, len(paths))
	for index, item := range paths {
		if item.mounted == "" {
			continue
		}
		contents, err := mountedSecretFile(secret, source, item.mounted)
		if err != nil {
			cleanup()
			return nil, func() {}, err
		}
		path := filepath.Join(directory, fmt.Sprintf("material-%d", index))
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("prepare controller TLS material")
		}
		options = append(options, item.apply(path))
	}
	return options, cleanup, nil
}

func mountedSecretFile(secret *corev1.Secret, source *corev1.SecretVolumeSource, mountedPath string) ([]byte, error) {
	relative, err := filepath.Rel(controllerCloudsMountPath, filepath.Clean(mountedPath))
	if err != nil || relative == "." || relative == "" || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return nil, fmt.Errorf("controller TLS file is outside the approved Secret mount")
	}
	key := relative
	if len(source.Items) != 0 {
		key = ""
		for _, item := range source.Items {
			if filepath.Clean(item.Path) == relative {
				key = item.Key
				break
			}
		}
	}
	if key == "" || len(secret.Data[key]) == 0 {
		return nil, fmt.Errorf("controller TLS file is absent from the approved Secret")
	}
	return secret.Data[key], nil
}

func authenticatedProjectIDFromProvider(provider *gophercloud.ProviderClient) (string, error) {
	switch result := provider.GetAuthResult().(type) {
	case tokensv3.CreateResult:
		project, err := result.ExtractProject()
		if err != nil {
			return "", fmt.Errorf("read authenticated controller project")
		}
		if project != nil && project.ID != "" {
			return project.ID, nil
		}
	case tokensv3.GetResult:
		project, err := result.ExtractProject()
		if err != nil {
			return "", fmt.Errorf("read authenticated controller project")
		}
		if project != nil && project.ID != "" {
			return project.ID, nil
		}
	case tokensv2.CreateResult:
		token, err := result.ExtractToken()
		if err != nil {
			return "", fmt.Errorf("read authenticated controller project")
		}
		if token.Tenant.ID != "" {
			return token.Tenant.ID, nil
		}
	}
	return "", fmt.Errorf("controller authentication was not project scoped")
}

func resolveAuthenticatedAuditProjectID(ctx context.Context, config auditConfig) (string, error) {
	serviceClients, err := openstack.NewServiceClients(ctx, openstack.ClientConfig{
		Region:         config.region,
		Microversion:   config.microversion,
		CloudsYAMLPath: config.cloudsYAML,
		CloudName:      config.cloud,
		APIQPS:         openstack.DefaultAPIQPS,
		APIBurst:       openstack.DefaultAPIBurst,
	})
	if err != nil {
		return "", fmt.Errorf("authenticate the audit credential")
	}
	return serviceClients.ProjectID, nil
}

func (s *phase2Suite) verifyAuthenticatedSharedProject(ctx context.Context) error {
	if s.config.project.mode != projectModeShared {
		return nil
	}
	if s.authenticatedProjectID == nil {
		return fmt.Errorf("shared-project authentication verifier is unavailable")
	}
	projectID, err := s.authenticatedProjectID(ctx, s.config.audit)
	if err != nil || projectID == "" || projectID != s.config.project.expectedProjectID {
		return fmt.Errorf("authenticated OpenStack project did not match the approved shared-project scope")
	}
	return nil
}
