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

	providerReasonAuthenticationFailed = "AuthenticationFailed"
	providerReasonAuthorizationFailed  = "AuthorizationFailed"
	providerReasonQuotaExceeded        = "QuotaExceeded"
	providerReasonRateLimited          = "RateLimited"
	providerReasonRequestTimedOut      = "RequestTimedOut"
	providerReasonUnavailable          = "ProviderUnavailable"
	providerReasonInvalidConfiguration = "InvalidProviderConfiguration"
	providerReasonResourceFailed       = "ResourceFailed"
	providerReasonOwnershipConflict    = "OwnershipConflict"
)

type providerFailurePolicy struct {
	category                   cloud.ErrorCategory
	conditionStatus            metav1.ConditionStatus
	reason                     string
	message                    string
	returnError                bool
	requeueAfter               time.Duration
	emitEvent                  bool
	advancesObservedGeneration bool
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

func providerProgressRequeueAfter(outcome cloud.Outcome, uid types.UID) time.Duration {
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

func openStackResyncAfter(interval time.Duration, uid types.UID) time.Duration {
	return stableJitter(interval, string(uid)+"/resync")
}

func providerProgressMessage(outcome cloud.Outcome, fallback string) string {
	if message := strings.TrimSpace(outcome.Message); message != "" {
		return message
	}
	return fallback
}

func providerFailurePolicyFor(err error) (providerFailurePolicy, bool) {
	category, ok := cloud.ErrorCategoryOf(err)
	if !ok {
		return providerFailurePolicy{}, false
	}

	policy := providerFailurePolicy{category: category}
	switch category {
	case cloud.ErrorCategoryAuthentication:
		policy.conditionStatus = metav1.ConditionUnknown
		policy.reason = providerReasonAuthenticationFailed
		policy.message = "The controller could not authenticate to OpenStack"
		policy.requeueAfter = providerFailureRequeue
		policy.emitEvent = true
	case cloud.ErrorCategoryAuthorization:
		policy.conditionStatus = metav1.ConditionUnknown
		policy.reason = providerReasonAuthorizationFailed
		policy.message = "OpenStack denied the requested operation"
		policy.requeueAfter = providerFailureRequeue
		policy.emitEvent = true
	case cloud.ErrorCategoryQuota:
		policy.conditionStatus = metav1.ConditionFalse
		policy.reason = providerReasonQuotaExceeded
		policy.message = "OpenStack quota is insufficient for the requested resources"
		policy.requeueAfter = providerFailureRequeue
		policy.emitEvent = true
		policy.advancesObservedGeneration = true
	case cloud.ErrorCategoryRateLimit:
		policy.conditionStatus = metav1.ConditionUnknown
		policy.reason = providerReasonRateLimited
		policy.message = "OpenStack rate limited the requested operation"
		policy.returnError = true
	case cloud.ErrorCategoryTimeout:
		policy.conditionStatus = metav1.ConditionUnknown
		policy.reason = providerReasonRequestTimedOut
		policy.message = "The OpenStack request timed out"
		policy.returnError = true
	case cloud.ErrorCategoryRetryableService:
		policy.conditionStatus = metav1.ConditionUnknown
		policy.reason = providerReasonUnavailable
		policy.message = "OpenStack could not complete the request"
		policy.returnError = true
	case cloud.ErrorCategoryTerminalValidation:
		policy.conditionStatus = metav1.ConditionFalse
		policy.reason = providerReasonInvalidConfiguration
		policy.message = "OpenStack rejected the requested configuration"
		policy.emitEvent = true
		policy.advancesObservedGeneration = true
	case cloud.ErrorCategoryResourceFailure:
		policy.conditionStatus = metav1.ConditionFalse
		policy.reason = providerReasonResourceFailed
		policy.message = "An OpenStack resource entered an error state"
		policy.requeueAfter = providerFailureRequeue
		policy.emitEvent = true
		policy.advancesObservedGeneration = true
	case cloud.ErrorCategoryOwnershipConflict:
		policy.conditionStatus = metav1.ConditionFalse
		policy.reason = providerReasonOwnershipConflict
		policy.message = "The OpenStack resource identity does not match the controller binding"
		policy.requeueAfter = ownershipConflictRequeue
		policy.emitEvent = true
		policy.advancesObservedGeneration = true
	default:
		policy.conditionStatus = metav1.ConditionUnknown
		policy.reason = providerReasonUnavailable
		policy.message = "OpenStack could not complete the request"
		policy.returnError = true
	}
	return policy, true
}

func providerCleanupFailurePolicy(policy providerFailurePolicy, resourceKind string) providerFailurePolicy {
	switch policy.category {
	case cloud.ErrorCategoryAuthentication:
		policy.message = resourceKind + " cleanup cannot continue because OpenStack authentication failed"
	case cloud.ErrorCategoryAuthorization:
		policy.message = resourceKind + " cleanup cannot continue because OpenStack denied the delete operation"
	case cloud.ErrorCategoryQuota:
		policy.message = resourceKind + " cleanup cannot continue because OpenStack reported insufficient quota"
	case cloud.ErrorCategoryRateLimit:
		policy.message = resourceKind + " cleanup is waiting for the OpenStack rate limit to clear"
	case cloud.ErrorCategoryTimeout:
		policy.message = resourceKind + " cleanup is waiting after an OpenStack request timed out"
	case cloud.ErrorCategoryRetryableService:
		policy.message = resourceKind + " cleanup is waiting for OpenStack to become available"
	case cloud.ErrorCategoryTerminalValidation:
		policy.message = resourceKind + " cleanup cannot continue because OpenStack rejected the delete operation"
	case cloud.ErrorCategoryResourceFailure:
		policy.message = resourceKind + " cleanup cannot continue because an OpenStack resource is in an error state"
	case cloud.ErrorCategoryOwnershipConflict:
		policy.message = resourceKind + " cleanup cannot continue because the OpenStack resource identity does not match the controller binding"
	default:
		policy.message = resourceKind + " cleanup is waiting for OpenStack to complete the delete operation"
	}
	return policy
}

func safeProviderReconcileError(policy providerFailurePolicy, cause error) error {
	return &providerReconcileError{message: policy.message, cause: cause}
}

func providerFailureResult(policy providerFailurePolicy, cause error, key string) (ctrl.Result, error) {
	return providerFailureResultForOperation(policy, cause, key, false)
}

func providerFinalizationFailureResult(policy providerFailurePolicy, cause error, key string) (ctrl.Result, error) {
	return providerFailureResultForOperation(policy, cause, key, true)
}

func providerFailureResultForOperation(
	policy providerFailurePolicy,
	cause error,
	key string,
	finalizing bool,
) (ctrl.Result, error) {
	if policy.returnError {
		return ctrl.Result{}, safeProviderReconcileError(policy, cause)
	}
	requeueAfter := policy.requeueAfter
	if finalizing && requeueAfter == 0 {
		requeueAfter = providerFailureRequeue
	}
	if requeueAfter == 0 {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: stableJitter(requeueAfter, fmt.Sprintf("%s/%s", key, policy.category))}, nil
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
