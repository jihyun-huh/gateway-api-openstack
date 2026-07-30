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

package probe

import (
	"strings"
	"testing"
)

func TestParseRunFlags(t *testing.T) {
	t.Parallel()

	getenv := mapEnvironment(map[string]string{
		"OS_REGION_NAME":                     "RegionOne",
		"GATEWAY_OPENSTACK_VIP_SUBNET_ID":    "vip-subnet",
		"GATEWAY_OPENSTACK_MEMBER_ADDRESSES": "192.0.2.10, 192.0.2.11",
		"GATEWAY_OPENSTACK_MEMBER_PORT":      "32080",
		"GATEWAY_OPENSTACK_CLUSTER_ID":       "test-cluster",
		"GATEWAY_OPENSTACK_GATEWAY_UID":      "8a4a9230-4d10-4e2f-af0c-9e7fd243b47e",
	})

	cfg, err := ParseRunFlags(nil, getenv)
	if err != nil {
		t.Fatalf("ParseRunFlags() error = %v", err)
	}
	if got, want := len(cfg.MemberAddresses), 2; got != want {
		t.Fatalf("len(MemberAddresses) = %d, want %d", got, want)
	}
	if got, want := cfg.MemberPort, 32080; got != want {
		t.Fatalf("MemberPort = %d, want %d", got, want)
	}
	if got, want := cfg.Provider, "amphora"; got != want {
		t.Fatalf("Provider = %q, want %q", got, want)
	}
}

func TestParseRunFlagsRejectsUnsafeIncompleteInput(t *testing.T) {
	t.Parallel()

	_, err := ParseRunFlags(nil, mapEnvironment(nil))
	if err == nil {
		t.Fatal("ParseRunFlags() unexpectedly succeeded")
	}
	for _, expected := range []string{"--vip-subnet-id", "--member-addresses", "--cluster-id", "--gateway-uid"} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("error %q does not mention %s", err, expected)
		}
	}
}

func TestValidateRejectsInvalidPortsAndPaths(t *testing.T) {
	t.Parallel()

	base := Config{
		VIPSubnetID:      "vip-subnet",
		MemberAddresses:  []string{"192.0.2.10"},
		MemberPort:       32080,
		ListenerPort:     80,
		HealthPath:       "/healthz",
		MatchPath:        "/",
		RequestPath:      "/",
		ClusterID:        "cluster",
		GatewayNamespace: "default",
		GatewayName:      "gateway",
		GatewayUID:       "uid",
		StateFile:        "state.json",
		ReportFile:       "report.json",
		OperationTimeout: 1,
		PollInterval:     1,
		TrafficTimeout:   1,
	}

	badPort := base
	badPort.MemberPort = 0
	if err := badPort.Validate(); err == nil {
		t.Error("Validate() accepted member port 0")
	}

	badPath := base
	badPath.MatchPath = "not-absolute"
	if err := badPath.Validate(); err == nil {
		t.Error("Validate() accepted a relative match path")
	}

	sameOutput := base
	sameOutput.ReportFile = sameOutput.StateFile
	if err := sameOutput.Validate(); err == nil {
		t.Error("Validate() accepted the same state and report path")
	}
}

func mapEnvironment(values map[string]string) func(string) string {
	return func(name string) string {
		return values[name]
	}
}
