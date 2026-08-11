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

package cloud

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestProviderErrorClassificationSurvivesWrapping(t *testing.T) {
	cause := errors.New("request failed")
	tests := []struct {
		name     string
		category ErrorCategory
		target   error
	}{
		{name: "authentication", category: ErrorCategoryAuthentication, target: ErrAuthentication},
		{name: "authorization", category: ErrorCategoryAuthorization, target: ErrAuthorization},
		{name: "quota", category: ErrorCategoryQuota, target: ErrQuotaExceeded},
		{name: "rate limit", category: ErrorCategoryRateLimit, target: ErrRateLimited},
		{name: "timeout", category: ErrorCategoryTimeout, target: ErrTimeout},
		{name: "retryable service", category: ErrorCategoryRetryableService, target: ErrRetryableService},
		{name: "terminal validation", category: ErrorCategoryTerminalValidation, target: ErrTerminalValidation},
		{name: "resource failure", category: ErrorCategoryResourceFailure, target: ErrResourceFailure},
		{name: "ownership conflict", category: ErrorCategoryOwnershipConflict, target: ErrOwnershipConflict},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			classified := NewProviderError(test.category, cause)
			wrapped := fmt.Errorf("ensure resource: %w", classified)

			if !errors.Is(wrapped, test.target) {
				t.Fatalf("errors.Is(%v) = false, want true", test.target)
			}
			if !errors.Is(wrapped, cause) {
				t.Fatal("classified error does not retain its cause")
			}
			var providerError *ProviderError
			if !errors.As(wrapped, &providerError) {
				t.Fatal("errors.As() did not find ProviderError")
			}
			if providerError.Category() != test.category {
				t.Fatalf("ProviderError category = %q, want %q", providerError.Category(), test.category)
			}
			category, ok := ErrorCategoryOf(wrapped)
			if !ok || category != test.category {
				t.Fatalf("ErrorCategoryOf() = %q, %t, want %q, true", category, ok, test.category)
			}
		})
	}
}

func TestProviderErrorCategoriesDoNotOverlap(t *testing.T) {
	err := NewProviderError(ErrorCategoryAuthentication, errors.New("expired token"))
	if errors.Is(err, ErrAuthorization) {
		t.Fatal("authentication error also matched authorization")
	}
}

func TestProviderErrorSentinelSurvivesWrapping(t *testing.T) {
	err := fmt.Errorf("validate graph: %w", ErrOwnershipConflict)
	var providerError *ProviderError
	if !errors.As(err, &providerError) || providerError.Category() != ErrorCategoryOwnershipConflict {
		t.Fatalf("errors.As() ProviderError = %#v", providerError)
	}
	category, ok := ErrorCategoryOf(err)
	if !ok || category != ErrorCategoryOwnershipConflict {
		t.Fatalf("ErrorCategoryOf() = %q, %t, want %q, true", category, ok, ErrorCategoryOwnershipConflict)
	}
}

func TestOutcomeValidation(t *testing.T) {
	tests := []struct {
		name    string
		outcome Outcome
		wantErr bool
	}{
		{name: "ready", outcome: ReadyOutcome()},
		{name: "progressing", outcome: ProgressingOutcome("Octavia load balancer is pending", 3*time.Second)},
		{name: "progressing with zero interval", outcome: ProgressingOutcome("Octavia load balancer is pending", 0)},
		{name: "progressing with negative interval", outcome: ProgressingOutcome("Octavia load balancer is pending", -time.Second)},
		{name: "ready with requeue", outcome: Outcome{State: OutcomeReady, RequeueAfter: time.Second}, wantErr: true},
		{name: "unknown", outcome: Outcome{}, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.outcome.Validate(); (err != nil) != test.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func TestProviderResultConstructorsKeepProgressSeparateFromErrors(t *testing.T) {
	gateway := GatewayProgressingResult("Octavia load balancer is pending", 2*time.Second)
	if gateway.Outcome.State != OutcomeProgressing || gateway.Outcome.RequeueAfter != 2*time.Second {
		t.Fatalf("Gateway progressing result = %#v", gateway)
	}
	route := RouteProgressingResult("Octavia pool is pending", 4*time.Second)
	if route.Outcome.State != OutcomeProgressing || route.Outcome.RequeueAfter != 4*time.Second {
		t.Fatalf("Route progressing result = %#v", route)
	}

	readyGateway := GatewayReadyResult(GatewayState{LoadBalancerID: "lb-1"})
	if readyGateway.Outcome.State != OutcomeReady || readyGateway.State.LoadBalancerID != "lb-1" {
		t.Fatalf("Gateway ready result = %#v", readyGateway)
	}
	readyRoute := RouteReadyResult(RouteState{PoolID: "pool-1"})
	if readyRoute.Outcome.State != OutcomeReady || readyRoute.State.PoolID != "pool-1" {
		t.Fatalf("Route ready result = %#v", readyRoute)
	}
}
