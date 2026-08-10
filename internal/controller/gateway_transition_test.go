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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

func TestGatewayDeletionProgressRetainsFinalizerAndBinding(t *testing.T) {
	cfg := testConfig()
	now := metav1.Now()
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "default",
			Name:              "edge",
			UID:               "gateway-uid",
			Generation:        1,
			DeletionTimestamp: &now,
			Finalizers:        []string{cfg.gatewayFinalizer()},
			Annotations:       gatewayBindingAnnotations(cfg, "80"),
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "openstack",
			Listeners: []gatewayv1.Listener{{
				Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80,
			}},
		},
	}
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(gateway).
		WithObjects(gateway).
		Build()
	progressing := cloud.ProgressingOutcome("Deleting controller-owned listener", time.Hour)
	provider := &recordingProvider{gatewayDeleteOut: &progressing}
	reconciler := GatewayReconciler{
		Client:      kubeClient,
		APIReader:   kubeClient,
		Provider:    provider,
		Coordinator: &GraphCoordinator{},
		Config:      cfg,
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}}

	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != maximumProviderProgressRequeue {
		t.Fatalf("Reconcile() RequeueAfter = %s, want %s", result.RequeueAfter, maximumProviderProgressRequeue)
	}
	if len(provider.deletedGateways) != 1 {
		t.Fatalf("DeleteGateway calls = %d, want 1", len(provider.deletedGateways))
	}

	current := &gatewayv1.Gateway{}
	if err := kubeClient.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatalf("get Gateway after progressing deletion: %v", err)
	}
	if !controllerutil.ContainsFinalizer(current, cfg.gatewayFinalizer()) {
		t.Fatal("progressing deletion removed the Gateway finalizer")
	}
	for annotation, want := range gatewayBindingAnnotations(cfg, "80") {
		if got := current.Annotations[annotation]; got != want {
			t.Errorf("Gateway annotation %q = %q, want %q", annotation, got, want)
		}
	}
}
