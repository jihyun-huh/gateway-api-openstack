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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/controller/graph"
)

func TestGatewayRevalidatesFinalizerAfterWaitingForGraph(t *testing.T) {
	config := testConfig()
	coordinator := &graph.Coordinator{}
	class := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: config.ControllerName},
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "default",
			Name:        "edge",
			UID:         "gateway-1",
			Generation:  1,
			Finalizers:  []string{config.GatewayFinalizer()},
			Annotations: gatewayBindingAnnotations(config, "80"),
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(class.Name),
			Listeners:        []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}},
		},
	}
	kubeClient := indexedFakeClientBuilder(testScheme(t), config).WithObjects(class, gateway).Build()
	provider := &recordingProvider{}
	reconciler := &Reconciler{Client: kubeClient, Provider: provider, Coordinator: coordinator, APIReader: kubeClient, Config: config}

	release, err := graph.Acquire(context.Background(), coordinator, string(gateway.UID))
	if err != nil {
		t.Fatalf("acquire graph lock: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := reconciler.ensureGateway(context.Background(), gateway.DeepCopy())
		result <- err
	}()
	var changed gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), &changed); err != nil {
		t.Fatal(err)
	}
	changed.Finalizers = nil
	if err := kubeClient.Update(context.Background(), &changed); err != nil {
		t.Fatal(err)
	}
	release()

	if err := <-result; !errors.Is(err, errGatewayChanged) {
		t.Fatalf("ensureGateway() error = %v, want %v", err, errGatewayChanged)
	}
	if len(provider.gatewaySpecs) != 0 {
		t.Fatalf("EnsureGateway calls = %d, want 0", len(provider.gatewaySpecs))
	}
}
