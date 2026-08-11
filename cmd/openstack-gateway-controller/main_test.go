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

package main

import (
	"testing"
	"time"
)

func TestDurationEnvOr(t *testing.T) {
	const name = "GATEWAY_OPENSTACK_TEST_DURATION"

	t.Run("fallback", func(t *testing.T) {
		t.Setenv(name, "")
		got, err := durationEnvOr(name, time.Minute)
		if err != nil || got != time.Minute {
			t.Fatalf("durationEnvOr() = %s, %v, want 1m, nil", got, err)
		}
	})

	t.Run("environment", func(t *testing.T) {
		t.Setenv(name, "90s")
		got, err := durationEnvOr(name, time.Minute)
		if err != nil || got != 90*time.Second {
			t.Fatalf("durationEnvOr() = %s, %v, want 90s, nil", got, err)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		t.Setenv(name, "later")
		if _, err := durationEnvOr(name, time.Minute); err == nil {
			t.Fatal("durationEnvOr() unexpectedly accepted an invalid duration")
		}
	})
}

func TestFloat64EnvOr(t *testing.T) {
	const name = "GATEWAY_OPENSTACK_TEST_QPS"

	t.Setenv(name, "")
	if got, err := float64EnvOr(name, 10); err != nil || got != 10 {
		t.Fatalf("float64EnvOr() = %v, %v, want 10, nil", got, err)
	}
	t.Setenv(name, "12.5")
	if got, err := float64EnvOr(name, 10); err != nil || got != 12.5 {
		t.Fatalf("float64EnvOr() = %v, %v, want 12.5, nil", got, err)
	}
	t.Setenv(name, "many")
	if _, err := float64EnvOr(name, 10); err == nil {
		t.Fatal("float64EnvOr() unexpectedly accepted an invalid number")
	}
}

func TestIntEnvOr(t *testing.T) {
	const name = "GATEWAY_OPENSTACK_TEST_BURST"

	t.Setenv(name, "")
	if got, err := intEnvOr(name, 20); err != nil || got != 20 {
		t.Fatalf("intEnvOr() = %d, %v, want 20, nil", got, err)
	}
	t.Setenv(name, "30")
	if got, err := intEnvOr(name, 20); err != nil || got != 30 {
		t.Fatalf("intEnvOr() = %d, %v, want 30, nil", got, err)
	}
	t.Setenv(name, "many")
	if _, err := intEnvOr(name, 20); err == nil {
		t.Fatal("intEnvOr() unexpectedly accepted an invalid integer")
	}
}
