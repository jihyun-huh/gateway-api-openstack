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

package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayconsts "sigs.k8s.io/gateway-api/pkg/consts"

	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller/graph"
)

func TestGatewayMutationBoundaryChecksLiveGatewayAPIVersion(t *testing.T) {
	cfg := testConfig()
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	gateway := programmedTestGateway(cfg)
	cachedClient := indexedFakeClientBuilder(testScheme(t), cfg).WithObjects(gatewayClass, gateway).Build()
	liveReader := liveReaderWithUnsupportedVersion(cachedClient)
	provider := &recordingProvider{}
	reconciler := &Reconciler{
		Client:      cachedClient,
		APIReader:   liveReader,
		Provider:    provider,
		Coordinator: &graph.Coordinator{},
		Config:      cfg,
	}

	_, err := reconciler.ensureGateway(context.Background(), gateway.DeepCopy())
	if !errors.Is(err, controller.ErrUnsupportedGatewayAPIVersion) {
		t.Fatalf("ensureGateway() error = %v, want unsupported Gateway API version", err)
	}
	if len(provider.gatewaySpecs) != 0 {
		t.Fatalf("EnsureGateway calls = %d, want 0", len(provider.gatewaySpecs))
	}
}

func TestGatewayReconcileChecksLiveVersionBeforeBinding(t *testing.T) {
	cfg := testConfig()
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-uid", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "openstack",
			Listeners:        []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}},
		},
	}
	cachedClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(gateway).
		WithObjects(gatewayClass, gateway).
		Build()
	liveReader := liveReaderWithUnsupportedVersion(cachedClient)
	provider := &recordingProvider{}
	reconciler := &Reconciler{
		Client: cachedClient, APIReader: liveReader, Provider: provider,
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gateway),
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("live Gateway API version mismatch did not schedule a fallback recheck")
	}
	var got gatewayv1.Gateway
	if err := cachedClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), &got); err != nil {
		t.Fatal(err)
	}
	if controller.GatewayHasControllerBinding(cfg, &got) {
		t.Fatalf("Gateway binding = finalizers %#v, annotations %#v; want none", got.Finalizers, got.Annotations)
	}
	if len(provider.gatewaySpecs) != 0 || len(provider.deletedGateways) != 0 {
		t.Fatalf("provider calls = ensures %d, deletes %d; want none", len(provider.gatewaySpecs), len(provider.deletedGateways))
	}
}

func TestGatewayReconcileChecksLiveVersionBeforeReplacementCleanup(t *testing.T) {
	cfg := testConfig()
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	gateway := programmedTestGateway(cfg)
	gateway.Spec.Listeners[0].Port = 443
	cachedClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(gateway).
		WithObjects(gatewayClass, gateway).
		Build()
	liveReader := liveReaderWithUnsupportedVersion(cachedClient)
	provider := &recordingProvider{}
	reconciler := &Reconciler{
		Client: cachedClient, APIReader: liveReader, Provider: provider,
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gateway),
	}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(provider.deletedGateways) != 0 || len(provider.gatewaySpecs) != 0 {
		t.Fatalf("provider calls = deletes %d, ensures %d; want none", len(provider.deletedGateways), len(provider.gatewaySpecs))
	}
	var got gatewayv1.Gateway
	if err := cachedClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), &got); err != nil {
		t.Fatal(err)
	}
	if !controller.GatewayHasControllerBinding(cfg, &got) {
		t.Fatal("Gateway replacement cleanup removed the stored binding during a CRD version mismatch")
	}
}

func TestGatewayCleanupRechecksControllerHandoffWithSupportedBundle(t *testing.T) {
	cfg := testConfig()
	cachedClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 2},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "other.example/controller"},
	}
	liveClass := cachedClass.DeepCopy()
	liveClass.Spec.ControllerName = cfg.ControllerName
	SetCondition(&liveClass.Status.Conditions, Condition(
		string(gatewayv1.GatewayClassConditionStatusSupportedVersion),
		metav1.ConditionTrue,
		string(gatewayv1.GatewayClassReasonSupportedVersion),
		"Gateway API version is supported",
		liveClass.Generation,
	))
	gateway := programmedTestGateway(cfg)
	cachedClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(gateway).
		WithObjects(cachedClass, gateway).
		Build()
	liveReader := &unsupportedGatewayAPIVersionReader{
		Client:        cachedClient,
		gatewayClass:  liveClass,
		useClientCRDs: true,
	}
	provider := &recordingProvider{}
	reconciler := &Reconciler{
		Client: cachedClient, APIReader: liveReader, Provider: provider,
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gateway),
	})
	if !errors.Is(err, errGatewayChanged) {
		t.Fatalf("Reconcile() error = %v, want %v", err, errGatewayChanged)
	}
	if len(provider.deletedGateways) != 0 {
		t.Fatalf("DeleteGateway calls = %d, want 0", len(provider.deletedGateways))
	}
}

func TestGatewayBindingChecksLiveControllerOwnership(t *testing.T) {
	cfg := testConfig()
	cachedClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	liveClass := cachedClass.DeepCopy()
	liveClass.Spec.ControllerName = "other.example/controller"
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-uid", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "openstack",
			Listeners:        []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}},
		},
	}
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(gateway).
		WithObjects(cachedClass, gateway).
		Build()
	liveReader := &unsupportedGatewayAPIVersionReader{
		Client: kubeClient, gatewayClass: liveClass, useClientCRDs: true,
	}
	reconciler := &Reconciler{
		Client: kubeClient, APIReader: liveReader, Provider: &recordingProvider{},
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gateway),
	})
	if !errors.Is(err, errGatewayChanged) {
		t.Fatalf("Reconcile() error = %v, want %v", err, errGatewayChanged)
	}
	var got gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), &got); err != nil {
		t.Fatal(err)
	}
	if controller.GatewayHasControllerBinding(cfg, &got) {
		t.Fatal("Gateway received a binding after live controller handoff")
	}
}

func TestGatewayProviderMutationRechecksControllerOwnership(t *testing.T) {
	cfg := testConfig()
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	foreignClass := gatewayClass.DeepCopy()
	foreignClass.Spec.ControllerName = "other.example/controller"
	gateway := programmedTestGateway(cfg)
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithObjects(gatewayClass, gateway).
		Build()
	liveReader := &gatewayClassFlipReader{
		Client: kubeClient, className: gatewayClass.Name, afterFirst: foreignClass,
	}
	provider := &recordingProvider{}
	reconciler := &Reconciler{
		Client: kubeClient, APIReader: liveReader, Provider: provider,
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}

	if _, err := reconciler.ensureGateway(context.Background(), gateway); !errors.Is(err, errGatewayChanged) {
		t.Fatalf("ensureGateway() error = %v, want %v", err, errGatewayChanged)
	}
	if len(provider.gatewaySpecs) != 0 {
		t.Fatalf("EnsureGateway calls = %d, want 0 after live controller handoff", len(provider.gatewaySpecs))
	}
}

func TestGatewayProviderMutationRechecksBinding(t *testing.T) {
	cfg := testConfig()
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	gateway := programmedTestGateway(cfg)
	changedGateway := gateway.DeepCopy()
	changedGateway.Annotations[cfg.GatewayListenerPortAnnotation()] = "443"
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithObjects(gatewayClass, gateway).
		Build()
	liveReader := &gatewayBindingFlipReader{Client: kubeClient, afterFirst: changedGateway}
	provider := &recordingProvider{}
	reconciler := &Reconciler{
		Client: kubeClient, APIReader: liveReader, Provider: provider,
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}

	if _, err := reconciler.ensureGateway(context.Background(), gateway); !errors.Is(err, errGatewayChanged) {
		t.Fatalf("ensureGateway() error = %v, want %v", err, errGatewayChanged)
	}
	if len(provider.gatewaySpecs) != 0 {
		t.Fatalf("EnsureGateway calls = %d, want 0 after the binding changed", len(provider.gatewaySpecs))
	}
}

func TestGatewayBindingMutationRechecksControllerOwnership(t *testing.T) {
	cfg := testConfig()
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	foreignClass := gatewayClass.DeepCopy()
	foreignClass.Spec.ControllerName = "other.example/controller"
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-uid", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "openstack",
			Listeners:        []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}},
		},
	}
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithObjects(gatewayClass, gateway).
		Build()
	liveReader := &gatewayClassFlipReader{
		Client: kubeClient, className: gatewayClass.Name, afterFirst: foreignClass,
	}
	reconciler := &Reconciler{
		Client: kubeClient, APIReader: liveReader, Provider: &recordingProvider{},
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}

	if err := reconciler.bindGateway(context.Background(), gateway, "80"); !errors.Is(err, errGatewayChanged) {
		t.Fatalf("bindGateway() error = %v, want %v", err, errGatewayChanged)
	}
	var got gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), &got); err != nil {
		t.Fatal(err)
	}
	if controller.GatewayHasControllerBinding(cfg, &got) {
		t.Fatal("Gateway received a binding after live controller handoff")
	}
}

func unsupportedGatewayAPICRDs() []client.Object {
	definitions := supportedGatewayAPICRDs()
	for _, definition := range definitions {
		if definition.GetName() == "httproutes.gateway.networking.k8s.io" {
			definition.SetAnnotations(map[string]string{gatewayconsts.BundleVersionAnnotation: "v1.5.0"})
		}
	}
	return definitions
}

type gatewayClassFlipReader struct {
	client.Client
	mu         sync.Mutex
	className  string
	classGets  int
	afterFirst *gatewayv1.GatewayClass
}

type gatewayBindingFlipReader struct {
	client.Client
	mu          sync.Mutex
	gatewayGets int
	afterFirst  *gatewayv1.Gateway
}

func (r *gatewayBindingFlipReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	opts ...client.GetOption,
) error {
	if gateway, ok := object.(*gatewayv1.Gateway); ok && key == client.ObjectKeyFromObject(r.afterFirst) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.gatewayGets++
		if r.gatewayGets > 1 {
			r.afterFirst.DeepCopyInto(gateway)
			return nil
		}
	}
	return r.Client.Get(ctx, key, object, opts...)
}

func (r *gatewayClassFlipReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	opts ...client.GetOption,
) error {
	if gatewayClass, ok := object.(*gatewayv1.GatewayClass); ok && key.Name == r.className {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.classGets++
		if r.classGets > 1 {
			r.afterFirst.DeepCopyInto(gatewayClass)
			return nil
		}
	}
	return r.Client.Get(ctx, key, object, opts...)
}

type unsupportedGatewayAPIVersionReader struct {
	client.Client
	gatewayClass  *gatewayv1.GatewayClass
	useClientCRDs bool
}

func (r *unsupportedGatewayAPIVersionReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	opts ...client.GetOption,
) error {
	gatewayClass, ok := object.(*gatewayv1.GatewayClass)
	if ok && r.gatewayClass != nil && key.Name == r.gatewayClass.Name {
		r.gatewayClass.DeepCopyInto(gatewayClass)
		return nil
	}
	return r.Client.Get(ctx, key, object, opts...)
}

func (r *unsupportedGatewayAPIVersionReader) List(
	ctx context.Context,
	list client.ObjectList,
	opts ...client.ListOption,
) error {
	definitions, ok := list.(*apiextensionsv1.CustomResourceDefinitionList)
	metadata, metadataOK := list.(*metav1.PartialObjectMetadataList)
	if (!ok && !metadataOK) || r.useClientCRDs {
		return r.Client.List(ctx, list, opts...)
	}
	if ok {
		definitions.Items = nil
	}
	if metadataOK {
		metadata.Items = nil
	}
	for _, object := range unsupportedGatewayAPICRDs() {
		definition := object.(*apiextensionsv1.CustomResourceDefinition)
		if ok {
			definitions.Items = append(definitions.Items, *definition.DeepCopy())
		}
		if metadataOK {
			metadata.Items = append(metadata.Items, metav1.PartialObjectMetadata{
				ObjectMeta: *definition.ObjectMeta.DeepCopy(),
			})
		}
	}
	return nil
}

func liveReaderWithUnsupportedVersion(kubeClient client.Client) client.Reader {
	return &unsupportedGatewayAPIVersionReader{Client: kubeClient}
}
