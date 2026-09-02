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

	"github.com/jihyun-huh/gateway-api-openstack/test/e2e/internal/controllercontract"
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
	cleanupBlocked      bool
}

func loadBaseObjects(repositoryRoot string) (baseObjects, error) {
	namespace, err := loadBaseObject[corev1.Namespace](repositoryRoot, "config/default/namespace.yaml")
	if err != nil {
		return baseObjects{}, err
	}
	serviceAccount, err := loadBaseObject[corev1.ServiceAccount](repositoryRoot, "config/rbac/service_account.yaml")
	if err != nil {
		return baseObjects{}, err
	}
	clusterRole, err := loadBaseObject[rbacv1.ClusterRole](repositoryRoot, "config/rbac/cluster_role.yaml")
	if err != nil {
		return baseObjects{}, err
	}
	clusterRoleBinding, err := loadBaseObject[rbacv1.ClusterRoleBinding](repositoryRoot, "config/rbac/cluster_role_binding.yaml")
	if err != nil {
		return baseObjects{}, err
	}
	configMap, err := loadBaseObject[corev1.ConfigMap](repositoryRoot, "config/manager/controller-config.yaml")
	if err != nil {
		return baseObjects{}, err
	}
	deployment, err := loadBaseObject[appsv1.Deployment](repositoryRoot, "config/manager/deployment.yaml")
	if err != nil {
		return baseObjects{}, err
	}
	return baseObjects{
		namespace: namespace, serviceAccount: serviceAccount, clusterRole: clusterRole,
		clusterRoleBinding: clusterRoleBinding, configMap: configMap, deployment: deployment,
	}, nil
}

func loadBaseObject[T any](repositoryRoot, path string) (*T, error) {
	contents, err := os.ReadFile(filepath.Join(repositoryRoot, filepath.FromSlash(path)))
	if err != nil {
		return nil, fmt.Errorf("read controller base %s: %w", path, err)
	}
	object := new(T)
	if err := yaml.UnmarshalStrict(contents, object); err != nil {
		return nil, fmt.Errorf("decode controller base %s: %w", path, err)
	}
	return object, nil
}

func buildControllerObjects(base baseObjects, config resolvedConfig, cloudsYAML []byte) (controllerObjects, error) {
	if len(cloudsYAML) == 0 {
		return controllerObjects{}, fmt.Errorf("controller clouds.yaml must not be empty")
	}
	if base.namespace == nil || base.serviceAccount == nil || base.clusterRole == nil ||
		base.clusterRoleBinding == nil || base.configMap == nil || base.deployment == nil {
		return controllerObjects{}, fmt.Errorf("controller base is incomplete")
	}
	namespace, serviceAccount, clusterRole, clusterRoleBinding := buildIdentityObjects(base, config)
	configMap, secret := buildConfigurationObjects(base.configMap, config, cloudsYAML)
	deployment, err := buildControllerDeployment(base.deployment, config, serviceAccount, configMap, secret)
	if err != nil {
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

func buildIdentityObjects(
	base baseObjects,
	config resolvedConfig,
) (*corev1.Namespace, *corev1.ServiceAccount, *rbacv1.ClusterRole, *rbacv1.ClusterRoleBinding) {
	namespace := base.namespace.DeepCopy()
	namespace.Name = config.ControllerNamespace
	setRunAnnotation(namespace, config.RunID)

	serviceAccount := base.serviceAccount.DeepCopy()
	serviceAccount.Name = controllerServiceAccount
	serviceAccount.Namespace = config.ControllerNamespace
	setRunAnnotation(serviceAccount, config.RunID)

	clusterRole := base.clusterRole.DeepCopy()
	clusterRole.Name = config.ClusterRoleName
	setRunAnnotation(clusterRole, config.RunID)

	clusterRoleBinding := base.clusterRoleBinding.DeepCopy()
	clusterRoleBinding.Name = config.ClusterRoleBindingName
	clusterRoleBinding.RoleRef.Name = clusterRole.Name
	clusterRoleBinding.Subjects = []rbacv1.Subject{{
		Kind:      rbacv1.ServiceAccountKind,
		Name:      serviceAccount.Name,
		Namespace: config.ControllerNamespace,
	}}
	setRunAnnotation(clusterRoleBinding, config.RunID)
	return namespace, serviceAccount, clusterRole, clusterRoleBinding
}

func buildConfigurationObjects(
	baseConfigMap *corev1.ConfigMap,
	config resolvedConfig,
	cloudsYAML []byte,
) (*corev1.ConfigMap, *corev1.Secret) {
	immutable := true
	configMap := baseConfigMap.DeepCopy()
	configMap.Name = controllerConfigMapName
	configMap.Namespace = config.ControllerNamespace
	configMap.Immutable = &immutable
	setRunAnnotation(configMap, config.RunID)
	settings := controllercontract.Settings{
		ControllerName:    config.ControllerName,
		ClusterID:         config.Audit.ClusterID,
		VIPSubnetID:       config.Project.ExpectedVIPSubnetID,
		ExternalNetworkID: config.Project.ExpectedExternalNetworkID,
		MemberSubnetID:    config.Project.ExpectedMemberSubnetID,
		Cloud:             config.Audit.Cloud,
		Region:            config.Audit.Region,
	}
	configMap.Data = settings.Environment()

	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.Project.ControllerCloudsSecret,
			Namespace: config.ControllerNamespace,
		},
		Immutable: &immutable,
		Type:      corev1.SecretTypeOpaque,
		Data:      map[string][]byte{"clouds.yaml": append([]byte(nil), cloudsYAML...)},
	}
	setRunAnnotation(secret, config.RunID)
	return configMap, secret
}

func buildControllerDeployment(
	baseDeployment *appsv1.Deployment,
	config resolvedConfig,
	serviceAccount *corev1.ServiceAccount,
	configMap *corev1.ConfigMap,
	secret *corev1.Secret,
) (*appsv1.Deployment, error) {
	deployment := baseDeployment.DeepCopy()
	deployment.Name = config.ControllerDeployment
	deployment.Namespace = config.ControllerNamespace
	deployment.Spec.Replicas = pointer(config.ControllerReplicas)
	deployment.Spec.Template.Spec.ServiceAccountName = serviceAccount.Name
	setRunAnnotation(deployment, config.RunID)
	setRunAnnotation(&deployment.Spec.Template, config.RunID)
	containerIndex := -1
	for index := range deployment.Spec.Template.Spec.Containers {
		if deployment.Spec.Template.Spec.Containers[index].Name == config.ControllerContainer {
			containerIndex = index
			break
		}
	}
	if containerIndex < 0 {
		return nil, fmt.Errorf("controller base deployment lacks container %q", config.ControllerContainer)
	}
	container := &deployment.Spec.Template.Spec.Containers[containerIndex]
	container.Image = config.ControllerImage
	container.Command = nil
	container.Env = nil
	container.EnvFrom = []corev1.EnvFromSource{{
		ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: configMap.Name}},
	}}
	container.VolumeMounts = []corev1.VolumeMount{{
		Name:      controllerCloudsVolume,
		MountPath: controllercontract.CloudsMountPath,
		ReadOnly:  true,
	}}
	deployment.Spec.Template.Spec.Volumes = []corev1.Volume{{
		Name: controllerCloudsVolume,
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  secret.Name,
			DefaultMode: pointer(int32(0o440)),
			Items:       []corev1.KeyToPath{{Key: "clouds.yaml", Path: "clouds.yaml"}},
		}},
	}}
	if err := controllercontract.ValidateArguments(container.Args, config.Audit.Microversion); err != nil {
		return nil, err
	}
	if err := controllercontract.ValidateMetricsPorts(container.Ports); err != nil {
		return nil, err
	}
	return deployment, nil
}

func bindDeploymentSources(deployment *appsv1.Deployment, configMap *corev1.ConfigMap, secret *corev1.Secret) error {
	if deployment == nil || configMap == nil || secret == nil {
		return fmt.Errorf("controller configuration sources lack API identity")
	}
	configSource, err := controllercontract.SourceVersion(configMap.UID, configMap.ResourceVersion)
	if err != nil {
		return err
	}
	cloudsSource, err := controllercontract.SourceVersion(secret.UID, secret.ResourceVersion)
	if err != nil {
		return err
	}
	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = map[string]string{}
	}
	deployment.Spec.Template.Annotations[controllercontract.ConfigSourceAnnotation] = configSource
	deployment.Spec.Template.Annotations[controllercontract.CloudsSourceAnnotation] = cloudsSource
	return nil
}

func installController(ctx context.Context, kubeClient client.Client, objects controllerObjects, config resolvedConfig) (installation, error) {
	result := installation{runID: config.RunID, controllerNamespace: config.ControllerNamespace}

	for _, object := range []client.Object{
		objects.namespace,
		objects.serviceAccount,
		objects.clusterRole,
		objects.clusterRoleBinding,
		objects.configMap,
		objects.secret,
	} {
		if err := result.create(ctx, kubeClient, object); err != nil {
			return result, fmt.Errorf("create run-scoped controller object: %w", err)
		}
	}
	if err := bindDeploymentSources(objects.deployment, objects.configMap, objects.secret); err != nil {
		return result, err
	}
	result.deploymentAttempted = true
	if err := result.create(ctx, kubeClient, objects.deployment); err != nil {
		return result, fmt.Errorf("create run-scoped controller deployment: %w", err)
	}
	return result, nil
}

func (i *installation) create(ctx context.Context, kubeClient client.Client, object client.Object) error {
	createErr := kubeClient.Create(ctx, object)
	if createErr != nil {
		return i.resolveCreateError(ctx, kubeClient, object, createErr)
	}
	if err := i.readCreatedIdentityWhenNeeded(ctx, kubeClient, object); err != nil {
		return err
	}
	return i.record(object)
}

func (i *installation) resolveCreateError(
	ctx context.Context,
	kubeClient client.Client,
	object client.Object,
	createErr error,
) error {
	if apierrors.IsAlreadyExists(createErr) {
		i.cleanupBlocked = true
		return fmt.Errorf("create run-scoped controller object collided with a pre-existing object: %w", createErr)
	}
	fresh := object.DeepCopyObject().(client.Object)
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(object), fresh); err != nil {
		i.cleanupBlocked = true
		return fmt.Errorf("create run-scoped controller object had an unverified outcome: %w", createErr)
	}
	if err := i.record(fresh); err != nil {
		return fmt.Errorf("create run-scoped controller object had an ownership conflict: %w", createErr)
	}
	object.SetUID(fresh.GetUID())
	object.SetResourceVersion(fresh.GetResourceVersion())
	return fmt.Errorf("create run-scoped controller object returned an error after the exact object became visible: %w", createErr)
}

func (i *installation) readCreatedIdentityWhenNeeded(ctx context.Context, kubeClient client.Client, object client.Object) error {
	if object.GetUID() != "" && object.GetResourceVersion() != "" {
		return nil
	}
	fresh := object.DeepCopyObject().(client.Object)
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(object), fresh); err != nil {
		i.cleanupBlocked = true
		return fmt.Errorf("read created run-scoped controller object identity: %w", err)
	}
	object.SetUID(fresh.GetUID())
	object.SetResourceVersion(fresh.GetResourceVersion())
	return nil
}

func (i *installation) record(object client.Object) error {
	if object.GetUID() == "" || object.GetAnnotations()[controllercontract.RunAnnotation] != i.runID {
		i.cleanupBlocked = true
		return fmt.Errorf("created run-scoped controller object lacks exact ownership identity")
	}
	i.objects = append(i.objects, installedObject{object: object, uid: object.GetUID()})
	return nil
}

func (i installation) remove(ctx context.Context, kubeClient client.Client) error {
	if i.cleanupBlocked {
		return fmt.Errorf("refuse controller cleanup because a create outcome was not verified")
	}
	validated, err := i.validateAllBeforeCleanup(ctx, kubeClient)
	if err != nil {
		return err
	}
	if err := deleteInstalledObjects(ctx, kubeClient, validated); err != nil {
		return err
	}
	return i.waitForNamespaceRemoval(ctx, kubeClient)
}

func (i installation) validateAllBeforeCleanup(ctx context.Context, kubeClient client.Client) ([]installedObject, error) {
	validated := make([]installedObject, 0, len(i.objects))
	for _, installed := range i.objects {
		fresh, found, err := i.validateInstalledObject(ctx, kubeClient, installed)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		validated = append(validated, fresh)
	}
	return validated, nil
}

func (i installation) validateInstalledObject(
	ctx context.Context,
	kubeClient client.Client,
	installed installedObject,
) (installedObject, bool, error) {
	fresh := installed.object.DeepCopyObject().(client.Object)
	err := kubeClient.Get(ctx, client.ObjectKeyFromObject(installed.object), fresh)
	if apierrors.IsNotFound(err) {
		return installedObject{}, false, nil
	}
	if err != nil {
		return installedObject{}, false, fmt.Errorf("read run-scoped controller object before cleanup: %w", err)
	}
	if fresh.GetUID() == "" || fresh.GetUID() != installed.uid || fresh.GetAnnotations()[controllercontract.RunAnnotation] != i.runID {
		return installedObject{}, false, fmt.Errorf("refuse controller cleanup because object identity changed")
	}
	return installedObject{object: fresh, uid: fresh.GetUID()}, true, nil
}

func deleteInstalledObjects(ctx context.Context, kubeClient client.Client, installedObjects []installedObject) error {
	for index := len(installedObjects) - 1; index >= 0; index-- {
		installed := installedObjects[index]
		uid := installed.uid
		if err := kubeClient.Delete(ctx, installed.object, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete run-scoped controller object: %w", err)
		}
	}
	return nil
}

func (i installation) waitForNamespaceRemoval(ctx context.Context, kubeClient client.Client) error {
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
	annotations[controllercontract.RunAnnotation] = runID
	object.SetAnnotations(annotations)
}

func pointer[T any](value T) *T { return &value }
