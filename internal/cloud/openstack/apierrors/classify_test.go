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

package apierrors

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

func TestClassifyOpenStackError(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantCategory cloud.ErrorCategory
		unclassified bool
	}{
		{
			name:         "authentication",
			err:          responseError(http.StatusUnauthorized, nil),
			wantCategory: cloud.ErrorCategoryAuthentication,
		},
		{
			name:         "reauthentication",
			err:          &gophercloud.ErrUnableToReauthenticate{ErrOriginal: errors.New("expired token"), ErrReauth: errors.New("Keystone unavailable")},
			wantCategory: cloud.ErrorCategoryAuthentication,
		},
		{
			name:         "authorization",
			err:          responseError(http.StatusForbidden, []byte(`{"faultstring":"Policy does not allow this request"}`)),
			wantCategory: cloud.ErrorCategoryAuthorization,
		},
		{
			name:         "Octavia forbidden without a machine readable quota code",
			err:          responseError(http.StatusForbidden, []byte(`{"faultcode":"Client","faultstring":"Quota has been met for resources: load_balancer"}`)),
			wantCategory: cloud.ErrorCategoryAuthorization,
		},
		{
			name:         "Neutron quota",
			err:          responseError(http.StatusConflict, []byte(`{"NeutronError":{"type":"OverQuota","message":"Quota exceeded for resources: ['floatingip']","detail":""}}`)),
			wantCategory: cloud.ErrorCategoryQuota,
		},
		{
			name:         "legacy quota status",
			err:          responseError(http.StatusRequestEntityTooLarge, nil),
			wantCategory: cloud.ErrorCategoryQuota,
		},
		{
			name:         "rate limit",
			err:          responseError(http.StatusTooManyRequests, nil),
			wantCategory: cloud.ErrorCategoryRateLimit,
		},
		{
			name:         "OpenStack invalid token rate limit",
			err:          responseError(openStackInvalidTokenStatus, nil),
			wantCategory: cloud.ErrorCategoryRateLimit,
		},
		{
			name:         "service unavailable",
			err:          responseError(http.StatusServiceUnavailable, nil),
			wantCategory: cloud.ErrorCategoryRetryableService,
		},
		{
			name:         "deadline",
			err:          fmt.Errorf("list pools: %w", context.DeadlineExceeded),
			wantCategory: cloud.ErrorCategoryTimeout,
		},
		{
			name:         "gateway timeout",
			err:          responseError(http.StatusGatewayTimeout, nil),
			wantCategory: cloud.ErrorCategoryTimeout,
		},
		{
			name:         "Gophercloud timeout",
			err:          &gophercloud.ErrTimeOut{},
			wantCategory: cloud.ErrorCategoryTimeout,
		},
		{
			name:         "truncated response",
			err:          io.ErrUnexpectedEOF,
			wantCategory: cloud.ErrorCategoryRetryableService,
		},
		{
			name:         "temporary DNS failure",
			err:          &net.DNSError{Err: "temporary resolver failure", Name: "octavia.example.test"},
			wantCategory: cloud.ErrorCategoryRetryableService,
		},
		{
			name:         "unknown endpoint host",
			err:          &net.DNSError{Err: "no such host", Name: "octavia.invalid", IsNotFound: true},
			wantCategory: cloud.ErrorCategoryTerminalValidation,
		},
		{
			name:         "malformed endpoint",
			err:          &url.Error{Op: "Get", URL: "://bad", Err: errors.New("unsupported protocol scheme")},
			wantCategory: cloud.ErrorCategoryTerminalValidation,
		},
		{
			name:         "invalid input",
			err:          gophercloud.ErrInvalidInput{ErrMissingInput: gophercloud.ErrMissingInput{Argument: "listener_id"}, Value: ""},
			wantCategory: cloud.ErrorCategoryTerminalValidation,
		},
		{
			name:         "bad request",
			err:          responseError(http.StatusBadRequest, nil),
			wantCategory: cloud.ErrorCategoryTerminalValidation,
		},
		{
			name:         "unsupported provider operation",
			err:          responseError(http.StatusNotImplemented, nil),
			wantCategory: cloud.ErrorCategoryTerminalValidation,
		},
		{
			name: "rate limit after reauthentication",
			err: &gophercloud.ErrErrorAfterReauthentication{
				ErrOriginal: responseError(http.StatusTooManyRequests, nil),
			},
			wantCategory: cloud.ErrorCategoryRateLimit,
		},
		{
			name: "timeout while reauthenticating",
			err: &gophercloud.ErrUnableToReauthenticate{
				ErrOriginal: responseError(http.StatusUnauthorized, nil),
				ErrReauth:   context.DeadlineExceeded,
			},
			wantCategory: cloud.ErrorCategoryTimeout,
		},
		{
			name:         "not found needs lifecycle context",
			err:          responseError(http.StatusNotFound, nil),
			unclassified: true,
		},
		{
			name:         "conflict needs operation context",
			err:          responseError(http.StatusConflict, []byte(`{"faultstring":"Load balancer is immutable"}`)),
			unclassified: true,
		},
		{
			name:         "precondition needs revision context",
			err:          responseError(http.StatusPreconditionFailed, nil),
			unclassified: true,
		},
		{
			name:         "cancellation",
			err:          fmt.Errorf("observe listener: %w", context.Canceled),
			unclassified: true,
		},
		{
			name:         "unknown failure",
			err:          errors.New("unknown failure"),
			unclassified: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Classify(test.err)
			category, classified := cloud.ErrorCategoryOf(got)
			if test.unclassified {
				if classified {
					t.Fatalf("classifyOpenStackError() category = %q, want unclassified", category)
				}
				if got != test.err {
					t.Fatalf("classifyOpenStackError() replaced unclassified error %T", test.err)
				}
				return
			}
			if !classified || category != test.wantCategory {
				t.Fatalf("classifyOpenStackError() category = %q, %t, want %q", category, classified, test.wantCategory)
			}
		})
	}
}

func TestClassifyOpenStackErrorPreservesExistingCategory(t *testing.T) {
	want := cloud.NewProviderError(cloud.ErrorCategoryOwnershipConflict, errors.New("foreign resource"))
	if got := Classify(want); got != want {
		t.Fatalf("classifyOpenStackError() = %v, want original classified error", got)
	}
}

func TestClassifyOctaviaMutationConflict(t *testing.T) {
	response := responseError(http.StatusConflict, []byte(`{"faultstring":"Load balancer is immutable"}`))
	got := ClassifyOctaviaMutation(response)
	if !errors.Is(got, cloud.ErrRetryableService) {
		t.Fatalf("classifyOctaviaMutationError() = %v, want retryable service category", got)
	}
	status, _, ok := openStackResponse(got)
	if !ok || status != http.StatusConflict {
		t.Fatalf("classifyOctaviaMutationError() did not retain 409 response: %v", got)
	}
}

func responseError(status int, body []byte) error {
	return &gophercloud.ErrUnexpectedResponseCode{
		Expected: []int{http.StatusOK},
		Actual:   status,
		Body:     body,
	}
}
