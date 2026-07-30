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
	"os"
	"path/filepath"
	"testing"
)

func TestStateRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "state.json")
	state := State{
		Version: stateVersion,
		Identity: Identity{
			ClusterID:        "cluster-a",
			Controller:       controllerIdentifier,
			GatewayNamespace: "default",
			GatewayName:      "web",
			GatewayUID:       "uid",
		},
		LoadBalancerID: "load-balancer",
	}
	if err := SaveState(path, state); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("state permissions = %o, want %o", got, want)
	}

	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if got.LoadBalancerID != state.LoadBalancerID {
		t.Fatalf("LoadBalancerID = %q, want %q", got.LoadBalancerID, state.LoadBalancerID)
	}
}

func TestCreateStateRefusesToOverwrite(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	first := State{
		Version: stateVersion,
		Identity: Identity{
			ClusterID:        "cluster-a",
			Controller:       controllerIdentifier,
			GatewayNamespace: "default",
			GatewayName:      "web",
			GatewayUID:       "first",
		},
	}
	if err := CreateState(path, first); err != nil {
		t.Fatalf("CreateState() error = %v", err)
	}

	second := first
	second.Identity.GatewayUID = "second"
	if err := CreateState(path, second); err == nil {
		t.Fatal("CreateState() overwrote an existing journal")
	}

	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if got.Identity.GatewayUID != "first" {
		t.Fatalf("GatewayUID = %q, want first", got.Identity.GatewayUID)
	}
}
