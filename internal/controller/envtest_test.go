//go:build envtest

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

package controller

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

const envtestWaitTimeout = 15 * time.Second

func TestControllerEnvtest(t *testing.T) {
	assetsDirectory := requiredEnvtestDirectory(t, "KUBEBUILDER_ASSETS")
	for _, binary := range []string{"kube-apiserver", "etcd", "kubectl"} {
		if _, err := os.Stat(filepath.Join(assetsDirectory, binary)); err != nil {
			t.Fatalf("read envtest binary %q: %v", binary, err)
		}
	}
	crdDirectory := requiredEnvtestDirectory(t, "GATEWAY_API_CRD_PATH")
	scheme := testScheme(t)

	testEnvironment := &envtest.Environment{
		Scheme:                   scheme,
		CRDDirectoryPaths:        []string{crdDirectory},
		ErrorIfCRDPathMissing:    true,
		DownloadBinaryAssets:     false,
		BinaryAssetsDirectory:    assetsDirectory,
		ControlPlaneStartTimeout: time.Minute,
		ControlPlaneStopTimeout:  time.Minute,
		AttachControlPlaneOutput: false,
	}
	restConfig, err := testEnvironment.Start()
	if err != nil {
		t.Fatalf("start envtest control plane: %v", err)
	}
	t.Cleanup(func() {
		if err := testEnvironment.Stop(); err != nil {
			t.Errorf("stop envtest control plane: %v", err)
		}
	})

	manager, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatalf("create envtest manager: %v", err)
	}
	config := testConfig()
	if err := SetupIndexes(t.Context(), manager.GetFieldIndexer(), config); err != nil {
		t.Fatalf("set up controller indexes: %v", err)
	}

	managerContext, stopManager := context.WithCancel(context.Background())
	managerResult := make(chan error, 1)
	go func() {
		managerResult <- manager.Start(managerContext)
	}()
	t.Cleanup(func() {
		stopManager()
		select {
		case err := <-managerResult:
			if err != nil {
				t.Errorf("stop envtest manager: %v", err)
			}
		case <-time.After(time.Minute):
			t.Error("envtest manager did not stop")
		}
	})
	if synced := manager.GetCache().WaitForCacheSync(t.Context()); !synced {
		select {
		case err := <-managerResult:
			t.Fatalf("start envtest manager: %v", err)
		default:
			t.Fatal("envtest manager cache did not sync")
		}
	}

	apiClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create envtest API client: %v", err)
	}
	cacheClient := manager.GetClient()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "controller-envtest"}}
	if err := apiClient.Create(t.Context(), namespace); err != nil {
		t.Fatalf("create test Namespace: %v", err)
	}

	// These phases share one API server scenario. Each phase leaves the durable
	// state that the next phase needs to exercise.
	gatewayClass := createEnvtestGatewayClass(t, apiClient, cacheClient, config)
	testGatewayClassStatusSubresource(t, apiClient, cacheClient, config, gatewayClass)
	testGatewayBindingConflict(t, apiClient, cacheClient, config, namespace.Name, gatewayClass.Name)

	gateway := createEnvtestGateway(t, apiClient, cacheClient, namespace.Name, "edge", gatewayClass.Name)
	testGatewayBindingCheckpoint(t, apiClient, cacheClient, config, gateway)
	testCacheFieldIndexes(t, apiClient, cacheClient, gateway)
	testFinalizersWithNewReconciler(t, apiClient, cacheClient, config, gatewayClass, gateway)
}

func requiredEnvtestDirectory(t *testing.T, environmentName string) string {
	t.Helper()
	directory := os.Getenv(environmentName)
	if directory == "" {
		t.Fatalf("%s is required; prepare assets with make envtest-assets and run make test-envtest", environmentName)
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("read directory from %s: %v", environmentName, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s path %q is not a directory", environmentName, directory)
	}
	return directory
}

func createEnvtestGatewayClass(
	t *testing.T,
	apiClient client.Client,
	cacheClient client.Client,
	config Config,
) *gatewayv1.GatewayClass {
	t.Helper()
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack-envtest"},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: config.ControllerName,
		},
	}
	if err := apiClient.Create(t.Context(), gatewayClass); err != nil {
		t.Fatalf("create GatewayClass: %v", err)
	}

	current := &gatewayv1.GatewayClass{}
	if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(gatewayClass), current); err != nil {
		t.Fatalf("get GatewayClass for status seed: %v", err)
	}
	current.Status.Conditions = []metav1.Condition{{
		Type:               "example.net/ExternalReady",
		Status:             metav1.ConditionTrue,
		Reason:             "ExternalController",
		Message:            "Preserve this condition",
		ObservedGeneration: current.Generation,
		LastTransitionTime: metav1.Now(),
	}}
	if err := apiClient.Status().Update(t.Context(), current); err != nil {
		t.Fatalf("seed GatewayClass status: %v", err)
	}
	waitForGatewayClassResourceVersion(t, cacheClient, current.Name, current.ResourceVersion)
	return current
}

func testGatewayClassStatusSubresource(
	t *testing.T,
	apiClient client.Client,
	cacheClient client.Client,
	config Config,
	gatewayClass *gatewayv1.GatewayClass,
) {
	t.Helper()
	beforeResourceVersion := gatewayClass.ResourceVersion
	reconciler := &GatewayClassReconciler{
		Client:    cacheClient,
		APIReader: apiClient,
		Config:    config,
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: gatewayClass.Name}}
	if _, err := reconciler.Reconcile(t.Context(), request); err != nil {
		t.Fatalf("reconcile GatewayClass: %v", err)
	}

	current := &gatewayv1.GatewayClass{}
	if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(gatewayClass), current); err != nil {
		t.Fatalf("get reconciled GatewayClass: %v", err)
	}
	if current.ResourceVersion == beforeResourceVersion {
		t.Fatal("GatewayClass status patch did not advance resourceVersion")
	}
	if current.Spec.ControllerName != config.ControllerName {
		t.Fatalf("GatewayClass controllerName = %q, want %q", current.Spec.ControllerName, config.ControllerName)
	}
	accepted := meta.FindStatusCondition(current.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionTrue || accepted.ObservedGeneration != current.Generation {
		t.Fatalf("GatewayClass Accepted condition = %#v", accepted)
	}
	supportedVersion := meta.FindStatusCondition(current.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion))
	if supportedVersion == nil || supportedVersion.Status != metav1.ConditionTrue {
		t.Fatalf("GatewayClass SupportedVersion condition = %#v", supportedVersion)
	}
	if external := meta.FindStatusCondition(current.Status.Conditions, "example.net/ExternalReady"); external == nil || external.Reason != "ExternalController" {
		t.Fatalf("foreign GatewayClass condition was not preserved: %#v", external)
	}

	waitForGatewayClassResourceVersion(t, cacheClient, current.Name, current.ResourceVersion)
	convergedResourceVersion := current.ResourceVersion
	if _, err := reconciler.Reconcile(t.Context(), request); err != nil {
		t.Fatalf("reconcile converged GatewayClass: %v", err)
	}
	if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(current), current); err != nil {
		t.Fatalf("get converged GatewayClass: %v", err)
	}
	if current.ResourceVersion != convergedResourceVersion {
		t.Fatalf("converged GatewayClass resourceVersion = %s, want %s", current.ResourceVersion, convergedResourceVersion)
	}
	*gatewayClass = *current
}

func createEnvtestGateway(
	t *testing.T,
	apiClient client.Client,
	cacheClient client.Client,
	namespace string,
	name string,
	className string,
) *gatewayv1.Gateway {
	t.Helper()
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(className),
			Listeners: []gatewayv1.Listener{{
				Name:     "http",
				Protocol: gatewayv1.HTTPProtocolType,
				Port:     80,
			}},
		},
	}
	if err := apiClient.Create(t.Context(), gateway); err != nil {
		t.Fatalf("create Gateway: %v", err)
	}
	waitForGatewayResourceVersion(t, cacheClient, client.ObjectKeyFromObject(gateway), gateway.ResourceVersion, false)
	return gateway
}

func testGatewayBindingConflict(
	t *testing.T,
	apiClient client.Client,
	cacheClient client.Client,
	config Config,
	namespace string,
	className string,
) {
	t.Helper()
	gateway := createEnvtestGateway(t, apiClient, cacheClient, namespace, "binding-conflict", className)
	provider := &recordingProvider{}
	conflictClient := &gatewayBindingConflictClient{
		Client:    cacheClient,
		apiClient: apiClient,
		key:       client.ObjectKeyFromObject(gateway),
		config:    config,
	}
	reconciler := &GatewayReconciler{
		Client:      conflictClient,
		APIReader:   apiClient,
		Provider:    provider,
		Coordinator: &GraphCoordinator{},
		Config:      config,
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gateway)}
	_, err := reconciler.Reconcile(t.Context(), request)
	if !apierrors.IsConflict(err) {
		t.Fatalf("Gateway binding error = %v, want resourceVersion conflict", err)
	}
	if !conflictClient.injected {
		t.Fatal("binding test did not submit a stale patch to the API server")
	}
	if len(provider.gatewaySpecs) != 0 || len(provider.deletedGateways) != 0 {
		t.Fatalf("provider mutations after binding conflict = ensures %d, deletes %d", len(provider.gatewaySpecs), len(provider.deletedGateways))
	}

	current := &gatewayv1.Gateway{}
	if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(gateway), current); err != nil {
		t.Fatalf("get Gateway after binding conflict: %v", err)
	}
	if current.Annotations["example.net/external-writer"] != "preserve" {
		t.Fatalf("external annotation = %q, want preserve", current.Annotations["example.net/external-writer"])
	}
	if controllerutil.ContainsFinalizer(current, config.gatewayFinalizer()) ||
		current.Annotations[config.gatewayClusterIDAnnotation()] != "" ||
		current.Annotations[config.gatewayProjectIDAnnotation()] != "" ||
		current.Annotations[config.gatewayListenerPortAnnotation()] != "" {
		t.Fatalf("stale binding was persisted: finalizers %#v, annotations %#v", current.Finalizers, current.Annotations)
	}
	if err := apiClient.Delete(t.Context(), current); err != nil {
		t.Fatalf("delete conflict test Gateway: %v", err)
	}
	waitForGatewayDeletion(t, cacheClient, client.ObjectKeyFromObject(current))
}

func testGatewayBindingCheckpoint(
	t *testing.T,
	apiClient client.Client,
	cacheClient client.Client,
	config Config,
	gateway *gatewayv1.Gateway,
) {
	t.Helper()
	provider := &recordingProvider{}
	reconciler := &GatewayReconciler{
		Client:      cacheClient,
		APIReader:   apiClient,
		Provider:    provider,
		Coordinator: &GraphCoordinator{},
		Config:      config,
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gateway)}
	result, err := reconciler.Reconcile(t.Context(), request)
	if err != nil {
		t.Fatalf("reconcile Gateway binding checkpoint: %v", err)
	}
	if result != (ctrl.Result{Requeue: true}) {
		t.Fatalf("Gateway binding result = %#v, want immediate requeue", result)
	}
	if len(provider.gatewaySpecs) != 0 || len(provider.deletedGateways) != 0 {
		t.Fatalf("provider mutations before binding checkpoint = ensures %d, deletes %d", len(provider.gatewaySpecs), len(provider.deletedGateways))
	}

	current := &gatewayv1.Gateway{}
	if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(gateway), current); err != nil {
		t.Fatalf("get bound Gateway: %v", err)
	}
	if !controllerutil.ContainsFinalizer(current, config.gatewayFinalizer()) {
		t.Fatal("Gateway finalizer was not persisted")
	}
	if current.Annotations[config.gatewayListenerPortAnnotation()] != "80" ||
		current.Annotations[config.gatewayClusterIDAnnotation()] != config.ClusterID ||
		current.Annotations[config.gatewayProjectIDAnnotation()] != config.OpenStackProjectID {
		t.Fatalf("Gateway binding annotations = %#v", current.Annotations)
	}
	waitForGatewayResourceVersion(t, cacheClient, client.ObjectKeyFromObject(current), current.ResourceVersion, false)
	*gateway = *current
}

func testCacheFieldIndexes(
	t *testing.T,
	apiClient client.Client,
	cacheClient client.Client,
	gateway *gatewayv1.Gateway,
) {
	t.Helper()
	parentKey := client.ObjectKeyFromObject(gateway).String()
	var gateways gatewayv1.GatewayList
	if err := cacheClient.List(t.Context(), &gateways, client.MatchingFields{
		indexGatewayByClass: string(gateway.Spec.GatewayClassName),
	}); err != nil {
		t.Fatalf("list Gateways through class index: %v", err)
	}
	if len(gateways.Items) != 1 || gateways.Items[0].Name != gateway.Name {
		t.Fatalf("class index returned Gateways %#v", objectNames(gateways.Items))
	}

	route := envtestHTTPRoute(gateway.Namespace, "api", gateway.Name, "backend")
	unrelated := envtestHTTPRoute(gateway.Namespace, "unrelated", "other", "other-backend")
	if err := apiClient.Create(t.Context(), route); err != nil {
		t.Fatalf("create indexed HTTPRoute: %v", err)
	}
	if err := apiClient.Create(t.Context(), unrelated); err != nil {
		t.Fatalf("create unrelated HTTPRoute: %v", err)
	}

	waitForIndexedHTTPRoute(t, cacheClient, indexHTTPRouteByParentGateway, parentKey, route.Name)
	waitForIndexedHTTPRoute(
		t,
		cacheClient,
		indexHTTPRouteByBackendService,
		client.ObjectKey{Namespace: route.Namespace, Name: "backend"}.String(),
		route.Name,
	)
}

func testFinalizersWithNewReconciler(
	t *testing.T,
	apiClient client.Client,
	cacheClient client.Client,
	config Config,
	gatewayClass *gatewayv1.GatewayClass,
	gateway *gatewayv1.Gateway,
) {
	t.Helper()
	classReconciler := &GatewayClassReconciler{Client: cacheClient, APIReader: apiClient, Config: config}
	classRequest := ctrl.Request{NamespacedName: types.NamespacedName{Name: gatewayClass.Name}}
	if _, err := classReconciler.Reconcile(t.Context(), classRequest); err != nil {
		t.Fatalf("add GatewayClass reference finalizer: %v", err)
	}
	currentClass := &gatewayv1.GatewayClass{}
	if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(gatewayClass), currentClass); err != nil {
		t.Fatalf("get finalized GatewayClass: %v", err)
	}
	if !controllerutil.ContainsFinalizer(currentClass, gatewayv1.GatewayClassFinalizerGatewaysExist) {
		t.Fatal("GatewayClass reference finalizer was not persisted through the cache index")
	}
	waitForGatewayClassResourceVersion(t, cacheClient, currentClass.Name, currentClass.ResourceVersion)
	if err := apiClient.Delete(t.Context(), currentClass); err != nil {
		t.Fatalf("delete GatewayClass: %v", err)
	}
	waitForGatewayClassDeletionTimestamp(t, cacheClient, currentClass.Name)

	if err := apiClient.Delete(t.Context(), gateway); err != nil {
		t.Fatalf("delete Gateway: %v", err)
	}
	waitForGatewayResourceVersion(t, cacheClient, client.ObjectKeyFromObject(gateway), "", true)

	progressing := cloud.ProgressingOutcome("OpenStack cleanup is progressing", time.Second)
	provider := &recordingProvider{}
	provider.gatewayDeleteOut = &progressing
	firstReconciler := &GatewayReconciler{
		Client:      cacheClient,
		APIReader:   apiClient,
		Provider:    provider,
		Coordinator: &GraphCoordinator{},
		Config:      config,
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gateway)}
	result, err := firstReconciler.Reconcile(t.Context(), request)
	if err != nil {
		t.Fatalf("observe progressing Gateway finalization: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("progressing Gateway finalization result = %#v", result)
	}
	currentGateway := &gatewayv1.Gateway{}
	if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(gateway), currentGateway); err != nil {
		t.Fatalf("get progressing Gateway deletion: %v", err)
	}
	if !controllerutil.ContainsFinalizer(currentGateway, config.gatewayFinalizer()) ||
		currentGateway.Annotations[config.gatewayClusterIDAnnotation()] == "" {
		t.Fatalf("progressing deletion dropped Gateway ownership: finalizers %#v, annotations %#v", currentGateway.Finalizers, currentGateway.Annotations)
	}

	ready := cloud.ReadyOutcome()
	provider.gatewayDeleteOut = &ready
	secondReconciler := &GatewayReconciler{
		Client:      cacheClient,
		APIReader:   apiClient,
		Provider:    provider,
		Coordinator: &GraphCoordinator{},
		Config:      config,
	}
	if _, err := secondReconciler.Reconcile(t.Context(), request); err != nil {
		t.Fatalf("resume Gateway finalization with a new reconciler: %v", err)
	}
	if len(provider.deletedGateways) != 2 {
		t.Fatalf("DeleteGateway calls = %d, want 2", len(provider.deletedGateways))
	}
	if provider.deletedGateways[0] != provider.deletedGateways[1] ||
		provider.deletedGateways[0].GatewayUID != string(gateway.UID) {
		t.Fatalf("Gateway deletion identities = %#v", provider.deletedGateways)
	}
	waitForGatewayDeletion(t, apiClient, client.ObjectKeyFromObject(gateway))
	waitForGatewayClassIndexEmpty(t, cacheClient, gatewayClass.Name)

	if _, err := classReconciler.Reconcile(t.Context(), classRequest); err != nil {
		t.Fatalf("remove GatewayClass reference finalizer: %v", err)
	}
	waitForGatewayClassDeletion(t, apiClient, currentClass.Name)
}

type gatewayBindingConflictClient struct {
	client.Client
	apiClient client.Client
	key       client.ObjectKey
	config    Config
	once      sync.Once
	injected  bool
	err       error
}

func (c *gatewayBindingConflictClient) Patch(
	ctx context.Context,
	object client.Object,
	patch client.Patch,
	options ...client.PatchOption,
) error {
	gateway, ok := object.(*gatewayv1.Gateway)
	if !ok || client.ObjectKeyFromObject(gateway) != c.key ||
		!controllerutil.ContainsFinalizer(gateway, c.config.gatewayFinalizer()) {
		return c.Client.Patch(ctx, object, patch, options...)
	}
	c.once.Do(func() {
		current := &gatewayv1.Gateway{}
		if err := c.apiClient.Get(ctx, c.key, current); err != nil {
			c.err = err
			return
		}
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations["example.net/external-writer"] = "preserve"
		c.err = c.apiClient.Update(ctx, current)
		c.injected = c.err == nil
	})
	if c.err != nil {
		return c.err
	}
	return c.Client.Patch(ctx, object, patch, options...)
}

func envtestHTTPRoute(namespace, name, parent, backend string) *gatewayv1.HTTPRoute {
	port := gatewayv1.PortNumber(80)
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{
				Name: gatewayv1.ObjectName(parent),
			}}},
			Rules: []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{
				BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
					Name: gatewayv1.ObjectName(backend),
					Port: &port,
				}},
			}}}},
		},
	}
}

func waitForGatewayClassResourceVersion(t *testing.T, reader client.Reader, name, resourceVersion string) {
	t.Helper()
	waitForEnvtest(t, func(ctx context.Context) (bool, error) {
		current := &gatewayv1.GatewayClass{}
		if err := reader.Get(ctx, client.ObjectKey{Name: name}, current); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return current.ResourceVersion == resourceVersion, nil
	})
}

func waitForGatewayClassDeletionTimestamp(t *testing.T, reader client.Reader, name string) {
	t.Helper()
	waitForEnvtest(t, func(ctx context.Context) (bool, error) {
		current := &gatewayv1.GatewayClass{}
		if err := reader.Get(ctx, client.ObjectKey{Name: name}, current); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return !current.DeletionTimestamp.IsZero(), nil
	})
}

func waitForGatewayClassIndexEmpty(t *testing.T, reader client.Reader, className string) {
	t.Helper()
	waitForEnvtest(t, func(ctx context.Context) (bool, error) {
		var gateways gatewayv1.GatewayList
		if err := reader.List(ctx, &gateways, client.MatchingFields{indexGatewayByClass: className}); err != nil {
			return false, err
		}
		return len(gateways.Items) == 0, nil
	})
}

func waitForGatewayClassDeletion(t *testing.T, reader client.Reader, name string) {
	t.Helper()
	waitForEnvtest(t, func(ctx context.Context) (bool, error) {
		err := reader.Get(ctx, client.ObjectKey{Name: name}, &gatewayv1.GatewayClass{})
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	})
}

func waitForGatewayResourceVersion(
	t *testing.T,
	reader client.Reader,
	key client.ObjectKey,
	resourceVersion string,
	wantDeleting bool,
) {
	t.Helper()
	waitForEnvtest(t, func(ctx context.Context) (bool, error) {
		current := &gatewayv1.Gateway{}
		if err := reader.Get(ctx, key, current); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		if wantDeleting {
			return !current.DeletionTimestamp.IsZero(), nil
		}
		return current.ResourceVersion == resourceVersion, nil
	})
}

func waitForIndexedHTTPRoute(
	t *testing.T,
	reader client.Reader,
	field string,
	value string,
	wantName string,
) {
	t.Helper()
	waitForEnvtest(t, func(ctx context.Context) (bool, error) {
		var routes gatewayv1.HTTPRouteList
		if err := reader.List(ctx, &routes, client.MatchingFields{field: value}); err != nil {
			return false, err
		}
		return len(routes.Items) == 1 && routes.Items[0].Name == wantName, nil
	})
}

func waitForGatewayDeletion(t *testing.T, reader client.Reader, key client.ObjectKey) {
	t.Helper()
	waitForEnvtest(t, func(ctx context.Context) (bool, error) {
		err := reader.Get(ctx, key, &gatewayv1.Gateway{})
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	})
}

func waitForEnvtest(t *testing.T, condition wait.ConditionWithContextFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), envtestWaitTimeout)
	defer cancel()
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, envtestWaitTimeout, true, condition); err != nil {
		t.Fatalf("wait for envtest state: %v", err)
	}
}

func objectNames(objects []gatewayv1.Gateway) []string {
	names := make([]string, 0, len(objects))
	for _, object := range objects {
		names = append(names, object.Namespace+"/"+object.Name)
	}
	return names
}
