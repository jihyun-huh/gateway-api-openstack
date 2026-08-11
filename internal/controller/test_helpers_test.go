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

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

type recordingProvider struct {
	gatewaySpecs     []cloud.GatewaySpec
	gatewayErr       error
	gatewayDeleteErr error
	gatewayGets      int
	routeSpecs       []cloud.RouteSpec
	routeDeleteErr   error
	deletedGateways  []cloud.Identity
	deletedRoutes    []cloud.Identity
}

func (p *recordingProvider) EnsureGateway(_ context.Context, spec cloud.GatewaySpec) (cloud.GatewayState, error) {
	p.gatewaySpecs = append(p.gatewaySpecs, spec)
	if p.gatewayErr != nil {
		return cloud.GatewayState{}, p.gatewayErr
	}
	return cloud.GatewayState{LoadBalancerID: "lb-1", VIPAddress: "192.0.2.10", ListenerID: "listener-1"}, nil
}

func (p *recordingProvider) GetGateway(context.Context, cloud.Identity) (cloud.GatewayState, bool, error) {
	p.gatewayGets++
	return cloud.GatewayState{LoadBalancerID: "lb-1", VIPAddress: "192.0.2.10", ListenerID: "listener-1"}, true, nil
}

func (p *recordingProvider) DeleteGateway(_ context.Context, identity cloud.Identity) error {
	p.deletedGateways = append(p.deletedGateways, identity)
	return p.gatewayDeleteErr
}

func (p *recordingProvider) EnsureRoute(_ context.Context, spec cloud.RouteSpec) (cloud.RouteState, error) {
	p.routeSpecs = append(p.routeSpecs, spec)
	return cloud.RouteState{PoolID: "pool-1"}, nil
}

func (p *recordingProvider) DeleteRoute(_ context.Context, identity cloud.Identity) error {
	p.deletedRoutes = append(p.deletedRoutes, identity)
	return p.routeDeleteErr
}

func testConfig() Config {
	return Config{
		ControllerName:     "example.com/gateway-api-openstack",
		ControllerVersion:  "test",
		OpenStackProjectID: "project-a",
		ClusterID:          "cluster-a",
		Provider:           "amphora",
		VIPSubnetID:        "vip-subnet",
		MemberSubnetID:     "member-subnet",
		MemberMode:         MemberModeNodePort,
		NodeAddressType:    corev1.NodeInternalIP,
		HealthPath:         "/",
	}
}

func gatewayBindingAnnotations(cfg Config, listenerPort string) map[string]string {
	return map[string]string{
		cfg.gatewayListenerPortAnnotation(): listenerPort,
		cfg.gatewayClusterIDAnnotation():    cfg.ClusterID,
		cfg.gatewayProjectIDAnnotation():    cfg.OpenStackProjectID,
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func indexedFakeClientBuilder(scheme *runtime.Scheme, config Config) *fake.ClientBuilder {
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, index := range controllerFieldIndexes(config) {
		if _, _, err := scheme.ObjectKinds(index.object); err != nil {
			continue
		}
		builder.WithIndex(index.object, index.field, index.extract)
	}
	return builder
}
