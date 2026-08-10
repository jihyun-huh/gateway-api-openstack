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

package openstack

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	gopheropenstack "github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

const openStackInvalidTokenStatus = 498

// classifyOpenStackError translates provider failures at the adapter boundary.
// A 404 remains unclassified because its meaning depends on the resource and
// lifecycle phase that issued the request.
func classifyOpenStackError(err error) error {
	if err == nil {
		return nil
	}
	if _, classified := cloud.ErrorCategoryOf(err); classified {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) || isGophercloudTimeout(err) {
		return cloud.NewProviderError(cloud.ErrorCategoryTimeout, err)
	}
	if original, ok := errorAfterReauthentication(err); ok {
		return classifyWrappedOpenStackError(err, original)
	}
	if reauthenticationErr, ok := reauthenticationFailure(err); ok {
		classified := classifyOpenStackError(reauthenticationErr)
		category, found := cloud.ErrorCategoryOf(classified)
		if found && (category == cloud.ErrorCategoryTimeout ||
			category == cloud.ErrorCategoryRateLimit ||
			category == cloud.ErrorCategoryRetryableService) {
			return cloud.NewProviderError(category, errors.Join(err, reauthenticationErr))
		}
		return cloud.NewProviderError(cloud.ErrorCategoryAuthentication, errors.Join(err, reauthenticationErr))
	}

	status, body, ok := openStackResponse(err)
	if ok {
		category, classified := categoryForResponse(status, body)
		if !classified {
			return err
		}
		return cloud.NewProviderError(category, err)
	}
	if isInvalidGophercloudInput(err) {
		return cloud.NewProviderError(cloud.ErrorCategoryTerminalValidation, err)
	}
	if category, classified := transportErrorCategory(err); classified {
		return cloud.NewProviderError(category, err)
	}
	return err
}

// classifyOctaviaMutationError handles the 409 contract documented by
// Octavia for a load balancer graph that has another action in progress. Read
// paths and Neutron conflicts keep their own lifecycle-specific handling.
func classifyOctaviaMutationError(err error) error {
	status, _, ok := openStackResponse(err)
	if err != nil && ok && status == http.StatusConflict {
		return cloud.NewProviderError(cloud.ErrorCategoryRetryableService, err)
	}
	return err
}

func categoryForResponse(status int, body []byte) (cloud.ErrorCategory, bool) {
	if (status == http.StatusForbidden || status == http.StatusConflict) && isNeutronQuotaResponse(body) {
		return cloud.ErrorCategoryQuota, true
	}
	switch status {
	case http.StatusUnauthorized:
		return cloud.ErrorCategoryAuthentication, true
	case http.StatusForbidden:
		return cloud.ErrorCategoryAuthorization, true
	case http.StatusNotFound:
		return "", false
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return cloud.ErrorCategoryTimeout, true
	case http.StatusConflict, http.StatusPreconditionFailed:
		return "", false
	case http.StatusRequestEntityTooLarge:
		return cloud.ErrorCategoryQuota, true
	case http.StatusTooManyRequests, openStackInvalidTokenStatus:
		return cloud.ErrorCategoryRateLimit, true
	case http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusNotAcceptable,
		http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity,
		http.StatusNotImplemented, http.StatusHTTPVersionNotSupported:
		return cloud.ErrorCategoryTerminalValidation, true
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable:
		return cloud.ErrorCategoryRetryableService, true
	}
	return "", false
}

func classifyWrappedOpenStackError(wrapper, cause error) error {
	classified := classifyOpenStackError(cause)
	category, ok := cloud.ErrorCategoryOf(classified)
	if !ok {
		return wrapper
	}
	return cloud.NewProviderError(category, errors.Join(wrapper, cause))
}

func openStackResponse(err error) (int, []byte, bool) {
	var response gophercloud.ErrUnexpectedResponseCode
	if errors.As(err, &response) {
		return response.Actual, response.Body, true
	}
	var responsePointer *gophercloud.ErrUnexpectedResponseCode
	if errors.As(err, &responsePointer) && responsePointer != nil {
		return responsePointer.Actual, responsePointer.Body, true
	}
	if original, ok := errorAfterReauthentication(err); ok {
		return openStackResponse(original)
	}
	return 0, nil, false
}

func isNeutronQuotaResponse(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var fault struct {
		NeutronError *struct {
			Type string `json:"type"`
		} `json:"NeutronError"`
	}
	if err := json.Unmarshal(body, &fault); err != nil {
		return false
	}
	return fault.NeutronError != nil && strings.EqualFold(strings.TrimSpace(fault.NeutronError.Type), "OverQuota")
}

func reauthenticationFailure(err error) (error, bool) {
	var unable gophercloud.ErrUnableToReauthenticate
	if errors.As(err, &unable) {
		return unable.ErrReauth, true
	}
	var unablePointer *gophercloud.ErrUnableToReauthenticate
	if errors.As(err, &unablePointer) && unablePointer != nil {
		return unablePointer.ErrReauth, true
	}
	return nil, false
}

func errorAfterReauthentication(err error) (error, bool) {
	var after gophercloud.ErrErrorAfterReauthentication
	if errors.As(err, &after) {
		return after.ErrOriginal, true
	}
	var afterPointer *gophercloud.ErrErrorAfterReauthentication
	if errors.As(err, &afterPointer) && afterPointer != nil {
		return afterPointer.ErrOriginal, true
	}
	return nil, false
}

func isGophercloudTimeout(err error) bool {
	var timeout gophercloud.ErrTimeOut
	if errors.As(err, &timeout) {
		return true
	}
	var timeoutPointer *gophercloud.ErrTimeOut
	return errors.As(err, &timeoutPointer) && timeoutPointer != nil
}

func isInvalidGophercloudInput(err error) bool {
	var invalid gophercloud.ErrInvalidInput
	if errors.As(err, &invalid) {
		return true
	}
	var invalidPointer *gophercloud.ErrInvalidInput
	if errors.As(err, &invalidPointer) && invalidPointer != nil {
		return true
	}
	var missing gophercloud.ErrMissingInput
	if errors.As(err, &missing) {
		return true
	}
	var serviceMissing gophercloud.ErrServiceNotFound
	if errors.As(err, &serviceMissing) {
		return true
	}
	var endpointMissing gophercloud.ErrEndpointNotFound
	if errors.As(err, &endpointMissing) {
		return true
	}
	var openStackEndpointMissing gopheropenstack.ErrEndpointNotFound
	return errors.As(err, &openStackEndpointMissing)
}

func transportErrorCategory(err error) (cloud.ErrorCategory, bool) {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return cloud.ErrorCategoryRetryableService, true
	}
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return cloud.ErrorCategoryTerminalValidation, true
	}
	var hostname x509.HostnameError
	if errors.As(err, &hostname) {
		return cloud.ErrorCategoryTerminalValidation, true
	}
	var certificateInvalid x509.CertificateInvalidError
	if errors.As(err, &certificateInvalid) {
		return cloud.ErrorCategoryTerminalValidation, true
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		if networkError.Timeout() {
			return cloud.ErrorCategoryTimeout, true
		}
		var dnsError *net.DNSError
		if errors.As(err, &dnsError) {
			if dnsError.IsNotFound {
				return cloud.ErrorCategoryTerminalValidation, true
			}
			return cloud.ErrorCategoryRetryableService, true
		}
		var operationError *net.OpError
		if errors.As(err, &operationError) {
			return cloud.ErrorCategoryRetryableService, true
		}
	}
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return cloud.ErrorCategoryTerminalValidation, true
	}
	return "", false
}
