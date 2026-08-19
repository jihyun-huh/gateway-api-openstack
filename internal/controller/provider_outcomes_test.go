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
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

func TestProviderProgressRequeueAfter(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{name: "default", want: defaultProviderProgressRequeue},
		{name: "negative", interval: -time.Second, want: defaultProviderProgressRequeue},
		{name: "minimum", interval: time.Millisecond, want: minimumProviderProgressRequeue},
		{name: "provider interval", interval: 4 * time.Second, want: 4 * time.Second},
		{name: "maximum", interval: time.Hour, want: maximumProviderProgressRequeue},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outcome := cloud.ProgressingOutcome("progressing", test.interval)
			want := stableJitter(test.want, "route-uid/progress")
			want = max(min(want, maximumProviderProgressRequeue), minimumProviderProgressRequeue)
			got := ProviderProgressRequeueAfter(outcome, types.UID("route-uid"))
			if got != want {
				t.Fatalf("ProviderProgressRequeueAfter() = %s, want %s", got, want)
			}
			if got < minimumProviderProgressRequeue || got > maximumProviderProgressRequeue {
				t.Fatalf("ProviderProgressRequeueAfter() = %s, want a delay from %s through %s", got, minimumProviderProgressRequeue, maximumProviderProgressRequeue)
			}
		})
	}
}

func TestOpenStackResyncAfterIsStableAndBounded(t *testing.T) {
	first := OpenStackResyncAfter(time.Minute, types.UID("route-uid"))
	second := OpenStackResyncAfter(time.Minute, types.UID("route-uid"))
	if first != second {
		t.Fatalf("OpenStackResyncAfter() = %s then %s, want a stable delay", first, second)
	}
	assertJitteredRequeue(t, first, time.Minute)
	if other := OpenStackResyncAfter(time.Minute, types.UID("different-route-uid")); other == first {
		t.Fatalf("different UIDs produced the same resync delay %s", first)
	}
}

func TestProviderProgressMessage(t *testing.T) {
	if got := ProviderProgressMessage(cloud.ProgressingOutcome("  Creating listener  ", time.Second), "fallback"); got != "Creating listener" {
		t.Fatalf("ProviderProgressMessage() = %q, want Creating listener", got)
	}
	if got := ProviderProgressMessage(cloud.ProgressingOutcome(" ", time.Second), "fallback"); got != "fallback" {
		t.Fatalf("ProviderProgressMessage() = %q, want fallback", got)
	}
}

func TestProviderFailurePolicy(t *testing.T) {
	tests := []struct {
		name            string
		category        cloud.ErrorCategory
		wantStatus      metav1.ConditionStatus
		wantReason      string
		wantMessage     string
		wantError       bool
		wantRequeueBase time.Duration
		wantEvent       bool
	}{
		{name: "authentication", category: cloud.ErrorCategoryAuthentication, wantStatus: metav1.ConditionUnknown, wantReason: "AuthenticationFailed", wantMessage: "The controller could not authenticate to OpenStack", wantRequeueBase: providerFailureRequeue, wantEvent: true},
		{name: "authorization", category: cloud.ErrorCategoryAuthorization, wantStatus: metav1.ConditionUnknown, wantReason: "AuthorizationFailed", wantMessage: "OpenStack denied the requested operation", wantRequeueBase: providerFailureRequeue, wantEvent: true},
		{name: "quota", category: cloud.ErrorCategoryQuota, wantStatus: metav1.ConditionFalse, wantReason: "QuotaExceeded", wantMessage: "OpenStack quota is insufficient for the requested resources", wantRequeueBase: providerFailureRequeue, wantEvent: true},
		{name: "rate limit", category: cloud.ErrorCategoryRateLimit, wantStatus: metav1.ConditionUnknown, wantReason: "RateLimited", wantMessage: "OpenStack rate limited the requested operation", wantError: true},
		{name: "timeout", category: cloud.ErrorCategoryTimeout, wantStatus: metav1.ConditionUnknown, wantReason: "RequestTimedOut", wantMessage: "The OpenStack request timed out", wantError: true},
		{name: "retryable service", category: cloud.ErrorCategoryRetryableService, wantStatus: metav1.ConditionUnknown, wantReason: "ProviderUnavailable", wantMessage: "OpenStack could not complete the request", wantError: true},
		{name: "terminal validation", category: cloud.ErrorCategoryTerminalValidation, wantStatus: metav1.ConditionFalse, wantReason: "InvalidProviderConfiguration", wantMessage: "OpenStack rejected the requested configuration", wantEvent: true},
		{name: "resource failure", category: cloud.ErrorCategoryResourceFailure, wantStatus: metav1.ConditionFalse, wantReason: "ResourceFailed", wantMessage: "An OpenStack resource entered an error state", wantRequeueBase: providerFailureRequeue, wantEvent: true},
		{name: "ownership conflict", category: cloud.ErrorCategoryOwnershipConflict, wantStatus: metav1.ConditionFalse, wantReason: "OwnershipConflict", wantMessage: "The OpenStack resource identity does not match the controller binding", wantRequeueBase: ownershipConflictRequeue, wantEvent: true},
		{name: "future category", category: cloud.ErrorCategory("FutureCategory"), wantStatus: metav1.ConditionUnknown, wantReason: "ProviderUnavailable", wantMessage: "OpenStack could not complete the request", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cause := errors.New("token=secret-token project=private-project response=private-body")
			providerErr := cloud.NewProviderError(test.category, cause)
			policy, ok := ProviderFailurePolicyFor(providerErr)
			if !ok {
				t.Fatal("ProviderFailurePolicyFor() did not classify a provider error")
			}
			if policy.ConditionStatus != test.wantStatus || policy.Reason != test.wantReason || policy.Message != test.wantMessage || policy.EmitEvent != test.wantEvent {
				t.Fatalf("policy = %#v", policy)
			}

			result, err := ProviderFailureResult(policy, providerErr, "object-uid")
			if test.wantError {
				if !errors.Is(err, providerErr) {
					t.Fatalf("ProviderFailureResult() error = %v, want wrapped provider error", err)
				}
				if err.Error() != test.wantMessage || strings.Contains(err.Error(), "secret-token") {
					t.Fatalf("ProviderFailureResult() error = %q", err)
				}
				if result != (ctrl.Result{}) {
					t.Fatalf("ProviderFailureResult() result = %#v, want empty result", result)
				}
				return
			}
			if err != nil {
				t.Fatalf("ProviderFailureResult() error = %v", err)
			}
			assertJitteredRequeue(t, result.RequeueAfter, test.wantRequeueBase)
		})
	}

	if _, ok := ProviderFailurePolicyFor(errors.New("unclassified internal error")); ok {
		t.Fatal("ProviderFailurePolicyFor() classified an ordinary internal error")
	}
	terminal := cloud.NewProviderError(cloud.ErrorCategoryTerminalValidation, errors.New("invalid"))
	policy, _ := ProviderFailurePolicyFor(terminal)
	result, err := ProviderFinalizationFailureResult(policy, terminal, "object-uid")
	if err != nil {
		t.Fatalf("ProviderFinalizationFailureResult() error = %v", err)
	}
	assertJitteredRequeue(t, result.RequeueAfter, providerFailureRequeue)
}

func assertJitteredRequeue(t *testing.T, got, base time.Duration) {
	t.Helper()
	if base == 0 {
		if got != 0 {
			t.Fatalf("RequeueAfter = %s, want zero", got)
		}
		return
	}
	if got < base*8/10 || got > base*12/10 {
		t.Fatalf("RequeueAfter = %s, want a value from %s through %s", got, base*8/10, base*12/10)
	}
}
