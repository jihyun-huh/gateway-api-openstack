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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestConfigFinalizersUseControllerDomain(t *testing.T) {
	cfg := Config{
		ControllerName:          gatewayv1.GatewayController("example.net/gateway-api-openstack"),
		ClusterID:               "cluster-a",
		Provider:                "amphora",
		VIPSubnetID:             "subnet-a",
		MemberSubnetID:          "member-subnet-a",
		MemberMode:              MemberModeNodePort,
		NodeAddressType:         corev1.NodeInternalIP,
		HealthPath:              "/healthz",
		OpenStackResyncInterval: time.Minute,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if got := cfg.GatewayFinalizer(); got != "example.net/gateway-finalizer-"+cfg.controllerKey() {
		t.Fatalf("gatewayFinalizer() = %q", got)
	}
	if got := cfg.RouteFinalizer(); got != "example.net/httproute-finalizer-"+cfg.controllerKey() {
		t.Fatalf("routeFinalizer() = %q", got)
	}
	if got := cfg.RouteCleanupFailureAnnotation(); got != "example.net/httproute-cleanup-failure-"+cfg.controllerKey() {
		t.Fatalf("routeCleanupFailureAnnotation() = %q", got)
	}
	other := cfg
	other.ControllerName = "example.net/another-controller"
	if cfg.GatewayFinalizer() == other.GatewayFinalizer() || cfg.RouteGatewayUIDAnnotation() == other.RouteGatewayUIDAnnotation() {
		t.Fatal("controller-scoped metadata keys collide for controllers sharing a DNS domain")
	}
}

func TestConfigValidateRejectsUnsupportedOrAmbiguousValues(t *testing.T) {
	valid := Config{
		ControllerName:          "example.net/gateway-api-openstack",
		ClusterID:               "cluster-a",
		Provider:                "amphora",
		VIPSubnetID:             "vip-subnet",
		MemberSubnetID:          "member-subnet",
		MemberMode:              MemberModeNodePort,
		NodeAddressType:         corev1.NodeInternalIP,
		HealthPath:              "/healthz",
		OpenStackResyncInterval: time.Minute,
	}
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "invalid controller name", mutate: func(cfg *Config) { cfg.ControllerName = "NOT_A_DOMAIN/controller" }},
		{name: "unverified provider", mutate: func(cfg *Config) { cfg.Provider = "ovn" }},
		{name: "implicit member mode", mutate: func(cfg *Config) { cfg.MemberMode = "" }},
		{name: "missing member subnet", mutate: func(cfg *Config) { cfg.MemberSubnetID = "" }},
		{name: "disabled OpenStack resync", mutate: func(cfg *Config) { cfg.OpenStackResyncInterval = 0 }},
		{name: "negative OpenStack resync", mutate: func(cfg *Config) { cfg.OpenStackResyncInterval = -time.Minute }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid
			test.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate() unexpectedly succeeded")
			}
		})
	}
}
