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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const (
	controllerConfigSourceAnnotation = "e2e.gateway-api-openstack.io/controller-config-source"
	controllerCloudsSourceAnnotation = "e2e.gateway-api-openstack.io/controller-clouds-source"
	controllerCloudsVolumeName       = "openstack-clouds"
	controllerCloudsMountPath        = "/etc/openstack"
)

type baseObjects struct {
	namespace          *corev1.Namespace
	serviceAccount     *corev1.ServiceAccount
	clusterRole        *rbacv1.ClusterRole
	clusterRoleBinding *rbacv1.ClusterRoleBinding
	configMap          *corev1.ConfigMap
	deployment         *appsv1.Deployment
}

type controllerObjects struct {
	namespace          *corev1.Namespace
	serviceAccount     *corev1.ServiceAccount
	clusterRole        *rbacv1.ClusterRole
	clusterRoleBinding *rbacv1.ClusterRoleBinding
	configMap          *corev1.ConfigMap
	secret             *corev1.Secret
	deployment         *appsv1.Deployment
}

type installedObject struct {
	object client.Object
	uid    types.UID
}

type installation struct {
	runID               string
	controllerNamespace string
	objects             []installedObject
	deploymentAttempted bool
}

func loadBaseObjects(repositoryRoot string) (baseObjects, error) {
	var result baseObjects
	readers := []struct {
		path   string
		target any
	}{
		{path: "config/default/namespace.yaml", target: &result.namespace},
		{path: "config/rbac/service_account.yaml", target: &result.serviceAccount},
		{path: "config/rbac/cluster_role.yaml", target: &result.clusterRole},
		{path: "config/rbac/cluster_role_binding.yaml", target: &result.clusterRoleBinding},
		{path: "config/manager/controller-config.yaml", target: &result.configMap},
		{path: "config/manager/deployment.yaml", target: &result.deployment},
	}
	for _, item := range readers {
		contents, err := os.ReadFile(filepath.Join(repositoryRoot, filepath.FromSlash(item.path)))
		if err != nil {
			return baseObjects{}, fmt.Errorf("read controller base %s: %w", item.path, err)
		}
		switch target := item.target.(type) {
		case **corev1.Namespace:
			object := &corev1.Namespace{}
			if err := yaml.UnmarshalStrict(contents, object); err != nil {
				return baseObjects{}, fmt.Errorf("decode controller base %s: %w", item.path, err)
			}
			*target = object
		case **corev1.ServiceAccount:
			object := &corev1.ServiceAccount{}
			if err := yaml.UnmarshalStrict(contents, object); err != nil {
				return baseObjects{}, fmt.Errorf("decode controller base %s: %w", item.path, err)
			}
			*target = object
		case **rbacv1.ClusterRole:
			object := &rbacv1.ClusterRole{}
			if err := yaml.UnmarshalStrict(contents, object); err != nil {
				return baseObjects{}, fmt.Errorf("decode controller base %s: %w", item.path, err)
			}
			*target = object
		case **rbacv1.ClusterRoleBinding:
			object := &rbacv1.ClusterRoleBinding{}
			if err := yaml.UnmarshalStrict(contents, object); err != nil {
				return baseObjects{}, fmt.Errorf("decode controller base %s: %w", item.path, err)
			}
			*target = object
		case **corev1.ConfigMap:
			object := &corev1.ConfigMap{}
			if err := yaml.UnmarshalStrict(contents, object); err != nil {
				return baseObjects{}, fmt.Errorf("decode controller base %s: %w", item.path, err)
			}
			*target = object
		case **appsv1.Deployment:
			object := &appsv1.Deployment{}
			if err := yaml.UnmarshalStrict(contents, object); err != nil {
				return baseObjects{}, fmt.Errorf("decode controller base %s: %w", item.path, err)
			}
			*target = object
		default:
			return baseObjects{}, fmt.Errorf("unsupported controller base object %s", item.path)
		}
	}
	return result, nil
}

func buildControllerObjects(base baseObjects, config resolvedConfig, cloudsYAML []byte) (controllerObjects, error) {
	if len(cloudsYAML) == 0 {
		return controllerObjects{}, fmt.Errorf("controller clouds.yaml must not be empty")
	}
	if base.namespace == nil || base.serviceAccount == nil || base.clusterRole == nil ||
		base.clusterRoleBinding == nil || base.configMap == nil || base.deployment == nil {
		return controllerObjects{}, fmt.Errorf("controller base is incomplete")
	}

	namespace := base.namespace.DeepCopy()
	namespace.Name = config.controllerNamespace
	setRunAnnotation(namespace, config.runID)

	serviceAccount := base.serviceAccount.DeepCopy()
	serviceAccount.Name = controllerServiceAccount
	serviceAccount.Namespace = config.controllerNamespace
	setRunAnnotation(serviceAccount, config.runID)

	clusterRole := base.clusterRole.DeepCopy()
	clusterRole.Name = config.clusterRoleName
	setRunAnnotation(clusterRole, config.runID)

	clusterRoleBinding := base.clusterRoleBinding.DeepCopy()
	clusterRoleBinding.Name = config.clusterRoleBindingName
	clusterRoleBinding.RoleRef.Name = clusterRole.Name
	clusterRoleBinding.Subjects = []rbacv1.Subject{{
		Kind:      rbacv1.ServiceAccountKind,
		Name:      serviceAccount.Name,
		Namespace: config.controllerNamespace,
	}}
	setRunAnnotation(clusterRoleBinding, config.runID)

	immutable := true
	configMap := base.configMap.DeepCopy()
	configMap.Name = controllerConfigMapName
	configMap.Namespace = config.controllerNamespace
	configMap.Immutable = &immutable
	setRunAnnotation(configMap, config.runID)
	if configMap.Data == nil {
		configMap.Data = map[string]string{}
	}
	for name, value := range map[string]string{
		"GATEWAY_OPENSTACK_CONTROLLER_NAME":     config.controllerName,
		"GATEWAY_OPENSTACK_CLUSTER_ID":          config.clusterID,
		"GATEWAY_OPENSTACK_OCTAVIA_PROVIDER":    "amphora",
		"GATEWAY_OPENSTACK_VIP_SUBNET_ID":       config.vipSubnetID,
		"GATEWAY_OPENSTACK_EXTERNAL_NETWORK_ID": config.externalNetworkID,
		"GATEWAY_OPENSTACK_MEMBER_SUBNET_ID":    config.memberSubnetID,
		"GATEWAY_OPENSTACK_MEMBER_MODE":         "NodePort",
		"GATEWAY_OPENSTACK_NODE_ADDRESS_TYPE":   "InternalIP",
		"OS_CLIENT_CONFIG_FILE":                 controllerCloudsMountPath + "/clouds.yaml",
		"OS_CLOUD":                              config.cloud,
		"OS_REGION_NAME":                        config.region,
	} {
		configMap.Data[name] = value
	}

	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      controllerSecretName,
			Namespace: config.controllerNamespace,
		},
		Immutable: &immutable,
		Type:      corev1.SecretTypeOpaque,
		Data:      map[string][]byte{"clouds.yaml": append([]byte(nil), cloudsYAML...)},
	}
	setRunAnnotation(secret, config.runID)

	deployment := base.deployment.DeepCopy()
	deployment.Name = controllerDeploymentName
	deployment.Namespace = config.controllerNamespace
	deployment.Spec.Replicas = pointer(controllerReplicas)
	deployment.Spec.Template.Spec.ServiceAccountName = serviceAccount.Name
	setRunAnnotation(deployment, config.runID)
	setRunAnnotation(&deployment.Spec.Template, config.runID)
	containerIndex := -1
	for index := range deployment.Spec.Template.Spec.Containers {
		if deployment.Spec.Template.Spec.Containers[index].Name == controllerContainerName {
			containerIndex = index
			break
		}
	}
	if containerIndex < 0 {
		return controllerObjects{}, fmt.Errorf("controller base deployment lacks container %q", controllerContainerName)
	}
	container := &deployment.Spec.Template.Spec.Containers[containerIndex]
	container.Image = config.controllerImage
	container.Command = nil
	container.Env = nil
	container.EnvFrom = []corev1.EnvFromSource{{
		ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: configMap.Name}},
	}}
	container.VolumeMounts = []corev1.VolumeMount{{
		Name:      controllerCloudsVolumeName,
		MountPath: controllerCloudsMountPath,
		ReadOnly:  true,
	}}
	deployment.Spec.Template.Spec.Volumes = []corev1.Volume{{
		Name: controllerCloudsVolumeName,
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  secret.Name,
			DefaultMode: pointer(int32(0o440)),
			Items:       []corev1.KeyToPath{{Key: "clouds.yaml", Path: "clouds.yaml"}},
		}},
	}}
	if err := validateControllerArguments(container.Args); err != nil {
		return controllerObjects{}, err
	}

	return controllerObjects{
		namespace:          namespace,
		serviceAccount:     serviceAccount,
		clusterRole:        clusterRole,
		clusterRoleBinding: clusterRoleBinding,
		configMap:          configMap,
		secret:             secret,
		deployment:         deployment,
	}, nil
}

func validateControllerArguments(arguments []string) error {
	protected := map[string]struct{}{
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
	want := map[string]string{
		"leader-elect":         "--leader-elect=true",
		"metrics-bind-address": "--metrics-bind-address=:8080",
		"octavia-microversion": "--octavia-microversion=" + octaviaMicroversion,
	}
	seen := make(map[string]int, len(want))
	for _, argument := range arguments {
		if !strings.HasPrefix(argument, "-") {
			continue
		}
		name := strings.TrimLeft(strings.SplitN(argument, "=", 2)[0], "-")
		if expected, found := want[name]; found {
			seen[name]++
			if argument != expected {
				return fmt.Errorf("controller base has an unsafe %s argument", name)
			}
			continue
		}
		if _, found := protected[name]; found {
			return fmt.Errorf("controller base overrides protected setting %s", name)
		}
	}
	for name := range want {
		if seen[name] != 1 {
			return fmt.Errorf("controller base must set %s exactly once", name)
		}
	}
	return nil
}

func bindDeploymentSources(deployment *appsv1.Deployment, configMap *corev1.ConfigMap, secret *corev1.Secret) error {
	if deployment == nil || configMap == nil || secret == nil || configMap.UID == "" || configMap.ResourceVersion == "" ||
		secret.UID == "" || secret.ResourceVersion == "" {
		return fmt.Errorf("controller configuration sources lack API identity")
	}
	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = map[string]string{}
	}
	deployment.Spec.Template.Annotations[controllerConfigSourceAnnotation] = string(configMap.UID) + "/" + configMap.ResourceVersion
	deployment.Spec.Template.Annotations[controllerCloudsSourceAnnotation] = string(secret.UID) + "/" + secret.ResourceVersion
	return nil
}

func installController(ctx context.Context, kubeClient client.Client, objects controllerObjects, config resolvedConfig) (installation, error) {
	result := installation{runID: config.runID, controllerNamespace: config.controllerNamespace}
	create := func(object client.Object) error {
		if err := kubeClient.Create(ctx, object); err != nil {
			return err
		}
		if object.GetUID() == "" || object.GetResourceVersion() == "" {
			fresh := object.DeepCopyObject().(client.Object)
			if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(object), fresh); err != nil {
				return err
			}
			object.SetUID(fresh.GetUID())
			object.SetResourceVersion(fresh.GetResourceVersion())
		}
		result.objects = append(result.objects, installedObject{object: object, uid: object.GetUID()})
		return nil
	}

	for _, object := range []client.Object{
		objects.namespace,
		objects.serviceAccount,
		objects.clusterRole,
		objects.clusterRoleBinding,
		objects.configMap,
		objects.secret,
	} {
		if err := create(object); err != nil {
			return result, fmt.Errorf("create run-scoped controller object: %w", err)
		}
	}
	if err := bindDeploymentSources(objects.deployment, objects.configMap, objects.secret); err != nil {
		return result, err
	}
	result.deploymentAttempted = true
	if err := create(objects.deployment); err != nil {
		return result, fmt.Errorf("create run-scoped controller deployment: %w", err)
	}
	return result, nil
}

func (i installation) remove(ctx context.Context, kubeClient client.Client) error {
	for index := len(i.objects) - 1; index >= 0; index-- {
		installed := i.objects[index]
		fresh := installed.object.DeepCopyObject().(client.Object)
		err := kubeClient.Get(ctx, client.ObjectKeyFromObject(installed.object), fresh)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read run-scoped controller object before cleanup: %w", err)
		}
		if fresh.GetUID() == "" || fresh.GetUID() != installed.uid || fresh.GetAnnotations()[runAnnotation] != i.runID {
			return fmt.Errorf("refuse controller cleanup because object identity changed")
		}
		uid := fresh.GetUID()
		if err := kubeClient.Delete(ctx, fresh, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete run-scoped controller object: %w", err)
		}
	}
	return wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		var namespace corev1.Namespace
		err := kubeClient.Get(ctx, client.ObjectKey{Name: i.controllerNamespace}, &namespace)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}

func setRunAnnotation(object metav1.Object, runID string) {
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[runAnnotation] = runID
	object.SetAnnotations(annotations)
}

func pointer[T any](value T) *T { return &value }
