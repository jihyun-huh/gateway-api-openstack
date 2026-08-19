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

package controller_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
)

type recordingProvider struct {
	gatewaySpecs     []cloud.GatewaySpec
	gatewayDeleteOut *cloud.Outcome
	deletedGateways  []cloud.Identity
}

func (p *recordingProvider) EnsureGateway(_ context.Context, spec cloud.GatewaySpec) (cloud.GatewayResult, error) {
	p.gatewaySpecs = append(p.gatewaySpecs, spec)
	return cloud.GatewayReadyResult(cloud.GatewayState{
		LoadBalancerID: "lb-1",
		VIPAddress:     "192.0.2.10",
		ListenerID:     "listener-1",
	}), nil
}

func (p *recordingProvider) GetGateway(context.Context, cloud.Identity) (cloud.GatewayResult, bool, error) {
	return cloud.GatewayReadyResult(cloud.GatewayState{
		LoadBalancerID: "lb-1",
		VIPAddress:     "192.0.2.10",
		ListenerID:     "listener-1",
	}), true, nil
}

func (p *recordingProvider) DeleteGateway(_ context.Context, identity cloud.Identity) (cloud.Outcome, error) {
	p.deletedGateways = append(p.deletedGateways, identity)
	if p.gatewayDeleteOut != nil {
		return *p.gatewayDeleteOut, nil
	}
	return cloud.ReadyOutcome(), nil
}

func (*recordingProvider) EnsureRoute(context.Context, cloud.RouteSpec) (cloud.RouteResult, error) {
	return cloud.RouteReadyResult(cloud.RouteState{PoolID: "pool-1"}), nil
}

func (*recordingProvider) DeleteRoute(context.Context, cloud.Identity) (cloud.Outcome, error) {
	return cloud.ReadyOutcome(), nil
}

func testConfig() controller.Config {
	return controller.Config{
		ControllerName:          "example.com/gateway-api-openstack",
		ControllerVersion:       "test",
		OpenStackProjectID:      "project-a",
		ClusterID:               "cluster-a",
		Provider:                "amphora",
		VIPSubnetID:             "vip-subnet",
		MemberSubnetID:          "member-subnet",
		MemberMode:              controller.MemberModeNodePort,
		NodeAddressType:         corev1.NodeInternalIP,
		HealthPath:              "/",
		OpenStackResyncInterval: time.Minute,
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
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}
