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
)

// ErrorCategory identifies the controller behavior appropriate for a provider
// failure. It remains available through ordinary Go error wrapping.
type ErrorCategory string

const (
	// ErrorCategoryAuthentication means the provider could not authenticate the
	// configured OpenStack credentials.
	ErrorCategoryAuthentication ErrorCategory = "Authentication"
	// ErrorCategoryAuthorization means the authenticated principal lacks a
	// required permission.
	ErrorCategoryAuthorization ErrorCategory = "Authorization"
	// ErrorCategoryQuota means the requested resource exceeds an OpenStack
	// quota.
	ErrorCategoryQuota ErrorCategory = "Quota"
	// ErrorCategoryRateLimit means OpenStack asked the client to reduce its
	// request rate.
	ErrorCategoryRateLimit ErrorCategory = "RateLimit"
	// ErrorCategoryTimeout means an OpenStack request exceeded its deadline.
	ErrorCategoryTimeout ErrorCategory = "Timeout"
	// ErrorCategoryRetryableService means a provider service failed
	// transiently.
	ErrorCategoryRetryableService ErrorCategory = "RetryableService"
	// ErrorCategoryTerminalValidation means the requested provider operation is
	// invalid and will not succeed without an input change.
	ErrorCategoryTerminalValidation ErrorCategory = "TerminalValidation"
	// ErrorCategoryResourceFailure means an asynchronous OpenStack resource
	// entered a terminal error state.
	ErrorCategoryResourceFailure ErrorCategory = "ResourceFailure"
	// ErrorCategoryOwnershipConflict means a discovered resource does not have
	// the complete immutable identity expected by this controller.
	ErrorCategoryOwnershipConflict ErrorCategory = "OwnershipConflict"
)

var (
	// ErrAuthentication classifies an OpenStack authentication failure.
	ErrAuthentication = &ProviderError{category: ErrorCategoryAuthentication}
	// ErrAuthorization classifies an OpenStack authorization failure.
	ErrAuthorization = &ProviderError{category: ErrorCategoryAuthorization}
	// ErrQuotaExceeded classifies OpenStack quota exhaustion.
	ErrQuotaExceeded = &ProviderError{category: ErrorCategoryQuota}
	// ErrRateLimited classifies an OpenStack rate limit response.
	ErrRateLimited = &ProviderError{category: ErrorCategoryRateLimit}
	// ErrTimeout classifies an OpenStack request that exceeded its deadline.
	ErrTimeout = &ProviderError{category: ErrorCategoryTimeout}
	// ErrRetryableService classifies a transient OpenStack service failure.
	ErrRetryableService = &ProviderError{category: ErrorCategoryRetryableService}
	// ErrTerminalValidation classifies an invalid provider operation that needs
	// an input change rather than workqueue backoff.
	ErrTerminalValidation = &ProviderError{category: ErrorCategoryTerminalValidation}
	// ErrResourceFailure classifies an OpenStack resource that entered a
	// terminal error state.
	ErrResourceFailure = &ProviderError{category: ErrorCategoryResourceFailure}
	// ErrOwnershipConflict classifies an immutable identity mismatch.
	ErrOwnershipConflict = &ProviderError{category: ErrorCategoryOwnershipConflict}
)

// ProviderError associates a provider failure with controller behavior while
// retaining its original cause.
type ProviderError struct {
	category ErrorCategory
	err      error
}

// Error returns a stable category message and the underlying cause, if any.
func (e *ProviderError) Error() string {
	message := errorCategoryMessage(e.category)
	if e.err == nil {
		return message
	}
	return fmt.Sprintf("%s: %v", message, e.err)
}

// Unwrap exposes the provider-specific cause.
func (e *ProviderError) Unwrap() error {
	return e.err
}

// Is compares provider errors by category so errors.Is works across wrapping
// and independently constructed errors.
func (e *ProviderError) Is(target error) bool {
	if other, ok := target.(*ProviderError); ok {
		return e.category == other.category
	}
	return false
}

// Category returns the controller behavior associated with the failure.
func (e *ProviderError) Category() ErrorCategory {
	return e.category
}

// NewProviderError classifies err without hiding it from errors.Is or
// errors.As.
func NewProviderError(category ErrorCategory, err error) *ProviderError {
	return &ProviderError{category: category, err: err}
}

// ErrorCategoryOf returns the first classified provider error in err's unwrap
// chain.
func ErrorCategoryOf(err error) (ErrorCategory, bool) {
	var providerError *ProviderError
	if errors.As(err, &providerError) {
		return providerError.category, true
	}
	return "", false
}

func errorCategoryMessage(category ErrorCategory) string {
	switch category {
	case ErrorCategoryAuthentication:
		return "OpenStack authentication failed"
	case ErrorCategoryAuthorization:
		return "OpenStack authorization failed"
	case ErrorCategoryQuota:
		return "OpenStack quota exceeded"
	case ErrorCategoryRateLimit:
		return "OpenStack rate limit exceeded"
	case ErrorCategoryTimeout:
		return "OpenStack request timed out"
	case ErrorCategoryRetryableService:
		return "retryable OpenStack service failure"
	case ErrorCategoryTerminalValidation:
		return "OpenStack request validation failed"
	case ErrorCategoryResourceFailure:
		return "OpenStack resource entered an error state"
	case ErrorCategoryOwnershipConflict:
		return "OpenStack resource ownership conflict"
	default:
		return "unclassified OpenStack provider failure"
	}
}
