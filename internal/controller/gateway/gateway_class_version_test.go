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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller/graph"
)

func TestGatewayClassVersionConditionFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
	}{
		{name: "condition is absent"},
		{
			name: "observed generation is stale",
			conditions: []metav1.Condition{Condition(
				string(gatewayv1.GatewayClassConditionStatusSupportedVersion),
				metav1.ConditionTrue,
				string(gatewayv1.GatewayClassReasonSupportedVersion),
				"Gateway API version is supported",
				1,
			)},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig()
			gatewayClass := &gatewayv1.GatewayClass{
				ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 2},
				Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
				Status:     gatewayv1.GatewayClassStatus{Conditions: test.conditions},
			}
			gateway := &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-uid", Generation: 1},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "openstack",
					Listeners:        []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}},
				},
			}
			objects := append([]client.Object{gatewayClass, gateway}, supportedGatewayAPICRDs()...)
			kubeClient := rawIndexedFakeClientBuilder(testScheme(t), cfg).
				WithStatusSubresource(gateway).
				WithObjects(objects...).
				Build()
			provider := &recordingProvider{}
			reconciler := &Reconciler{
				Client: kubeClient, APIReader: kubeClient, Provider: provider,
				Coordinator: &graph.Coordinator{}, Config: cfg,
			}

			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: client.ObjectKeyFromObject(gateway),
			}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			var got gatewayv1.Gateway
			if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), &got); err != nil {
				t.Fatal(err)
			}
			if controller.GatewayHasControllerBinding(cfg, &got) {
				t.Fatalf("Gateway received a binding with GatewayClass conditions %#v", test.conditions)
			}
			if len(provider.gatewaySpecs) != 0 || len(provider.deletedGateways) != 0 {
				t.Fatalf("provider calls = ensures %d, deletes %d; want none", len(provider.gatewaySpecs), len(provider.deletedGateways))
			}
		})
	}
}
