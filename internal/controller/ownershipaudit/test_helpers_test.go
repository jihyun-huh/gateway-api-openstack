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

package ownershipaudit

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
)

type Config = controller.Config

func testConfig() Config {
	return Config{
		ControllerName:     "example.com/gateway-api-openstack",
		ControllerVersion:  "test",
		OpenStackProjectID: "project-a",
		ClusterID:          "cluster-a",
		Provider:           "amphora",
		VIPSubnetID:        "vip-subnet",
		MemberSubnetID:     "member-subnet",
		MemberMode:         controller.MemberModeNodePort,
		NodeAddressType:    corev1.NodeInternalIP,
		HealthPath:         "/",
	}
}

func gatewayBindingAnnotations(cfg Config, listenerPort string) map[string]string {
	return map[string]string{
		cfg.GatewayListenerPortAnnotation(): listenerPort,
		cfg.GatewayClusterIDAnnotation():    cfg.ClusterID,
		cfg.GatewayProjectIDAnnotation():    cfg.OpenStackProjectID,
	}
}

func applyRouteBinding(cfg Config, route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) {
	route.Annotations = map[string]string{
		cfg.RouteGatewayNamespaceAnnotation(): gateway.Namespace,
		cfg.RouteGatewayNameAnnotation():      gateway.Name,
		cfg.RouteGatewayUIDAnnotation():       string(gateway.UID),
		cfg.RouteClusterIDAnnotation():        cfg.ClusterID,
		cfg.RouteProjectIDAnnotation():        cfg.OpenStackProjectID,
	}
	controllerutil.AddFinalizer(route, cfg.RouteBindingFinalizer(
		cfg.ClusterID,
		cfg.OpenStackProjectID,
		gateway.Namespace,
		gateway.Name,
		string(gateway.UID),
	))
	controllerutil.AddFinalizer(route, cfg.RouteFinalizer())
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}
