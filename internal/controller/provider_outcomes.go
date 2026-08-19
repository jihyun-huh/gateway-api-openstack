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
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

const (
	minimumProviderProgressRequeue = time.Second
	defaultProviderProgressRequeue = 5 * time.Second
	maximumProviderProgressRequeue = 30 * time.Second
	providerFailureRequeue         = 2 * time.Minute
	ownershipConflictRequeue       = 10 * time.Minute

	// ProviderReasonAuthenticationFailed identifies an OpenStack authentication failure.
	ProviderReasonAuthenticationFailed = "AuthenticationFailed"
	// ProviderReasonAuthorizationFailed identifies an OpenStack authorization failure.
	ProviderReasonAuthorizationFailed = "AuthorizationFailed"
	// ProviderReasonQuotaExceeded identifies insufficient OpenStack quota.
	ProviderReasonQuotaExceeded = "QuotaExceeded"
	// ProviderReasonRateLimited identifies OpenStack request throttling.
	ProviderReasonRateLimited = "RateLimited"
	// ProviderReasonRequestTimedOut identifies an OpenStack request timeout.
	ProviderReasonRequestTimedOut = "RequestTimedOut"
	// ProviderReasonUnavailable identifies a retryable OpenStack service failure.
	ProviderReasonUnavailable = "ProviderUnavailable"
	// ProviderReasonInvalidConfiguration identifies terminal provider validation.
	ProviderReasonInvalidConfiguration = "InvalidProviderConfiguration"
	// ProviderReasonResourceFailed identifies an OpenStack resource error state.
	ProviderReasonResourceFailed = "ResourceFailed"
	// ProviderReasonOwnershipConflict identifies an immutable identity mismatch.
	ProviderReasonOwnershipConflict = "OwnershipConflict"
)

// ProviderFailurePolicy describes how a classified provider error is reported
// and retried by a reconciler.
type ProviderFailurePolicy struct {
	// Category is the provider-neutral error classification.
	Category cloud.ErrorCategory
	// ConditionStatus is the status published on the owning API object.
	ConditionStatus metav1.ConditionStatus
	// Reason is the stable condition and Event reason.
	Reason string
	// Message is the redacted operator-facing explanation.
	Message string
	// ReturnError requests controller-runtime workqueue backoff.
	ReturnError bool
	// RequeueAfter requests an explicit retry when ReturnError is false.
	RequeueAfter time.Duration
	// EmitEvent requests a Warning Event for the failure.
	EmitEvent bool
	// AdvancesObservedGeneration reports whether the input was fully evaluated.
	AdvancesObservedGeneration bool
}

type providerReconcileError struct {
	message string
	cause   error
}

func (e *providerReconcileError) Error() string {
	return e.message
}

func (e *providerReconcileError) Unwrap() error {
	return e.cause
}

// ProviderProgressRequeueAfter returns a bounded, stable retry interval for an
// asynchronous provider operation.
func ProviderProgressRequeueAfter(outcome cloud.Outcome, uid types.UID) time.Duration {
	var interval time.Duration
	switch {
	case outcome.RequeueAfter <= 0:
		interval = defaultProviderProgressRequeue
	case outcome.RequeueAfter < minimumProviderProgressRequeue:
		interval = minimumProviderProgressRequeue
	case outcome.RequeueAfter > maximumProviderProgressRequeue:
		interval = maximumProviderProgressRequeue
	default:
		interval = outcome.RequeueAfter
	}
	requeueAfter := stableJitter(interval, string(uid)+"/progress")
	if requeueAfter < minimumProviderProgressRequeue {
		return minimumProviderProgressRequeue
	}
	if requeueAfter > maximumProviderProgressRequeue {
		return maximumProviderProgressRequeue
	}
	return requeueAfter
}

// OpenStackResyncAfter applies stable UID-based jitter to a cloud resync
// interval.
func OpenStackResyncAfter(interval time.Duration, uid types.UID) time.Duration {
	return stableJitter(interval, string(uid)+"/resync")
}

// ProviderProgressMessage returns the provider progress message, or fallback
// when the provider supplied no message.
func ProviderProgressMessage(outcome cloud.Outcome, fallback string) string {
	if message := strings.TrimSpace(outcome.Message); message != "" {
		return message
	}
	return fallback
}

// ProviderFailurePolicyFor maps a classified cloud error to controller retry
// and reporting behavior.
func ProviderFailurePolicyFor(err error) (ProviderFailurePolicy, bool) {
	category, ok := cloud.ErrorCategoryOf(err)
	if !ok {
		return ProviderFailurePolicy{}, false
	}

	policy := ProviderFailurePolicy{Category: category}
	switch category {
	case cloud.ErrorCategoryAuthentication:
		policy.ConditionStatus = metav1.ConditionUnknown
		policy.Reason = ProviderReasonAuthenticationFailed
		policy.Message = "The controller could not authenticate to OpenStack"
		policy.RequeueAfter = providerFailureRequeue
		policy.EmitEvent = true
	case cloud.ErrorCategoryAuthorization:
		policy.ConditionStatus = metav1.ConditionUnknown
		policy.Reason = ProviderReasonAuthorizationFailed
		policy.Message = "OpenStack denied the requested operation"
		policy.RequeueAfter = providerFailureRequeue
		policy.EmitEvent = true
	case cloud.ErrorCategoryQuota:
		policy.ConditionStatus = metav1.ConditionFalse
		policy.Reason = ProviderReasonQuotaExceeded
		policy.Message = "OpenStack quota is insufficient for the requested resources"
		policy.RequeueAfter = providerFailureRequeue
		policy.EmitEvent = true
		policy.AdvancesObservedGeneration = true
	case cloud.ErrorCategoryRateLimit:
		policy.ConditionStatus = metav1.ConditionUnknown
		policy.Reason = ProviderReasonRateLimited
		policy.Message = "OpenStack rate limited the requested operation"
		policy.ReturnError = true
	case cloud.ErrorCategoryTimeout:
		policy.ConditionStatus = metav1.ConditionUnknown
		policy.Reason = ProviderReasonRequestTimedOut
		policy.Message = "The OpenStack request timed out"
		policy.ReturnError = true
	case cloud.ErrorCategoryRetryableService:
		policy.ConditionStatus = metav1.ConditionUnknown
		policy.Reason = ProviderReasonUnavailable
		policy.Message = "OpenStack could not complete the request"
		policy.ReturnError = true
	case cloud.ErrorCategoryTerminalValidation:
		policy.ConditionStatus = metav1.ConditionFalse
		policy.Reason = ProviderReasonInvalidConfiguration
		policy.Message = "OpenStack rejected the requested configuration"
		policy.EmitEvent = true
		policy.AdvancesObservedGeneration = true
	case cloud.ErrorCategoryResourceFailure:
		policy.ConditionStatus = metav1.ConditionFalse
		policy.Reason = ProviderReasonResourceFailed
		policy.Message = "An OpenStack resource entered an error state"
		policy.RequeueAfter = providerFailureRequeue
		policy.EmitEvent = true
		policy.AdvancesObservedGeneration = true
	case cloud.ErrorCategoryOwnershipConflict:
		policy.ConditionStatus = metav1.ConditionFalse
		policy.Reason = ProviderReasonOwnershipConflict
		policy.Message = "The OpenStack resource identity does not match the controller binding"
		policy.RequeueAfter = ownershipConflictRequeue
		policy.EmitEvent = true
		policy.AdvancesObservedGeneration = true
	default:
		policy.ConditionStatus = metav1.ConditionUnknown
		policy.Reason = ProviderReasonUnavailable
		policy.Message = "OpenStack could not complete the request"
		policy.ReturnError = true
	}
	return policy, true
}

// ProviderCleanupFailurePolicy adapts a provider failure policy for a resource
// finalization diagnostic.
func ProviderCleanupFailurePolicy(policy ProviderFailurePolicy, resourceKind string) ProviderFailurePolicy {
	switch policy.Category {
	case cloud.ErrorCategoryAuthentication:
		policy.Message = resourceKind + " cleanup cannot continue because OpenStack authentication failed"
	case cloud.ErrorCategoryAuthorization:
		policy.Message = resourceKind + " cleanup cannot continue because OpenStack denied the delete operation"
	case cloud.ErrorCategoryQuota:
		policy.Message = resourceKind + " cleanup cannot continue because OpenStack reported insufficient quota"
	case cloud.ErrorCategoryRateLimit:
		policy.Message = resourceKind + " cleanup is waiting for the OpenStack rate limit to clear"
	case cloud.ErrorCategoryTimeout:
		policy.Message = resourceKind + " cleanup is waiting after an OpenStack request timed out"
	case cloud.ErrorCategoryRetryableService:
		policy.Message = resourceKind + " cleanup is waiting for OpenStack to become available"
	case cloud.ErrorCategoryTerminalValidation:
		policy.Message = resourceKind + " cleanup cannot continue because OpenStack rejected the delete operation"
	case cloud.ErrorCategoryResourceFailure:
		policy.Message = resourceKind + " cleanup cannot continue because an OpenStack resource is in an error state"
	case cloud.ErrorCategoryOwnershipConflict:
		policy.Message = resourceKind + " cleanup cannot continue because the OpenStack resource identity does not match the controller binding"
	default:
		policy.Message = resourceKind + " cleanup is waiting for OpenStack to complete the delete operation"
	}
	return policy
}

// SafeProviderReconcileError wraps a provider error with its redacted policy
// message while retaining the original error for classification.
func SafeProviderReconcileError(policy ProviderFailurePolicy, cause error) error {
	return &providerReconcileError{message: policy.Message, cause: cause}
}

// ProviderFailureResult converts a provider failure policy into a reconcile
// result for normal reconciliation.
func ProviderFailureResult(policy ProviderFailurePolicy, cause error, key string) (ctrl.Result, error) {
	return providerFailureResultForOperation(policy, cause, key, false)
}

// ProviderFinalizationFailureResult converts a provider failure policy into a
// reconcile result that keeps finalization progressing.
func ProviderFinalizationFailureResult(policy ProviderFailurePolicy, cause error, key string) (ctrl.Result, error) {
	return providerFailureResultForOperation(policy, cause, key, true)
}

func providerFailureResultForOperation(
	policy ProviderFailurePolicy,
	cause error,
	key string,
	finalizing bool,
) (ctrl.Result, error) {
	if policy.ReturnError {
		return ctrl.Result{}, SafeProviderReconcileError(policy, cause)
	}
	requeueAfter := policy.RequeueAfter
	if finalizing && requeueAfter == 0 {
		requeueAfter = providerFailureRequeue
	}
	if requeueAfter == 0 {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: stableJitter(requeueAfter, fmt.Sprintf("%s/%s", key, policy.Category))}, nil
}

// stableJitter spreads periodic work across a bounded interval while keeping
// the delay stable for a given scheduling key.
func stableJitter(base time.Duration, key string) time.Duration {
	if base <= 0 {
		return base
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(key))
	// Select a value from 80.0% through 120.0% in 0.1% steps.
	offsetPermille := int64(hash.Sum32()%401) - 200
	baseValue := int64(base)
	delta := baseValue/1000*offsetPermille + baseValue%1000*offsetPermille/1000
	const maximumDuration = time.Duration(1<<63 - 1)
	if delta > 0 && base > maximumDuration-time.Duration(delta) {
		return maximumDuration
	}
	return base + time.Duration(delta)
}
