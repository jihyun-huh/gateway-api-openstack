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

package clients

import (
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
)

// RequestService is a bounded OpenStack service classification used for
// request observations.
type RequestService string

const (
	// RequestServiceKeystone identifies OpenStack Identity API requests.
	RequestServiceKeystone RequestService = "keystone"
	// RequestServiceOctavia identifies OpenStack Load Balancing API requests.
	RequestServiceOctavia RequestService = "octavia"
	// RequestServiceNeutron identifies OpenStack Networking API requests.
	RequestServiceNeutron RequestService = "neutron"
	// RequestServiceUnknown identifies requests that do not match a discovered
	// service endpoint.
	RequestServiceUnknown RequestService = "unknown"
)

// RequestMethod is a bounded HTTP method classification used for request
// observations.
type RequestMethod string

const (
	// RequestMethodGet identifies GET requests.
	RequestMethodGet RequestMethod = "GET"
	// RequestMethodHead identifies HEAD requests.
	RequestMethodHead RequestMethod = "HEAD"
	// RequestMethodPost identifies POST requests.
	RequestMethodPost RequestMethod = "POST"
	// RequestMethodPut identifies PUT requests.
	RequestMethodPut RequestMethod = "PUT"
	// RequestMethodPatch identifies PATCH requests.
	RequestMethodPatch RequestMethod = "PATCH"
	// RequestMethodDelete identifies DELETE requests.
	RequestMethodDelete RequestMethod = "DELETE"
	// RequestMethodOptions identifies OPTIONS requests.
	RequestMethodOptions RequestMethod = "OPTIONS"
	// RequestMethodOther identifies methods outside the supported bounded set.
	RequestMethodOther RequestMethod = "OTHER"
)

// RequestStatusClass is a bounded response classification used for request
// observations.
type RequestStatusClass string

const (
	// RequestStatusClassInformational identifies HTTP 1xx responses.
	RequestStatusClassInformational RequestStatusClass = "1xx"
	// RequestStatusClassSuccessful identifies HTTP 2xx responses.
	RequestStatusClassSuccessful RequestStatusClass = "2xx"
	// RequestStatusClassRedirection identifies HTTP 3xx responses.
	RequestStatusClassRedirection RequestStatusClass = "3xx"
	// RequestStatusClassClientError identifies HTTP 4xx responses.
	RequestStatusClassClientError RequestStatusClass = "4xx"
	// RequestStatusClassServerError identifies HTTP 5xx responses.
	RequestStatusClassServerError RequestStatusClass = "5xx"
	// RequestStatusClassError identifies transport failures and invalid status
	// codes.
	RequestStatusClassError RequestStatusClass = "error"
)

// RequestStartObservation contains the bounded classifications available
// before an OpenStack request is sent.
type RequestStartObservation struct {
	Service RequestService
	Method  RequestMethod
}

// RequestObservation contains only bounded completion classifications. It
// intentionally excludes request URLs, resource IDs, headers, and bodies.
type RequestObservation struct {
	Service     RequestService
	Method      RequestMethod
	StatusClass RequestStatusClass
	Duration    time.Duration
}

// RequestObserver receives bounded observations immediately before an
// OpenStack request is sent and after it finishes. Implementations must be safe
// for concurrent use.
type RequestObserver interface {
	// ObserveRequestStart records a request admitted by the client-side rate
	// limiter immediately before the underlying transport starts it.
	ObserveRequestStart(RequestStartObservation)
	// ObserveRequest records the matching request completion.
	ObserveRequest(RequestObservation)
}

type requestObservingRoundTripper struct {
	next       http.RoundTripper
	classifier requestServiceClassifier
	observer   RequestObserver
}

func (t *requestObservingRoundTripper) RoundTrip(request *http.Request) (response *http.Response, err error) {
	service := t.classifier.classify(request)
	method := classifyRequestMethod(request)
	t.observer.ObserveRequestStart(RequestStartObservation{
		Service: service,
		Method:  method,
	})
	started := time.Now()
	defer func() {
		t.observer.ObserveRequest(RequestObservation{
			Service:     service,
			Method:      method,
			StatusClass: classifyStatus(response, err),
			Duration:    time.Since(started),
		})
	}()
	return t.next.RoundTrip(request)
}

func installRequestObserver(
	provider *gophercloud.ProviderClient,
	loadBalancerClient, networkClient *gophercloud.ServiceClient,
	observer RequestObserver,
) {
	observing := &requestObservingRoundTripper{
		classifier: newRequestServiceClassifier(
			serviceEndpoint{service: RequestServiceKeystone, rawURL: provider.IdentityEndpoint},
			serviceEndpoint{service: RequestServiceKeystone, rawURL: provider.IdentityBase},
			serviceEndpoint{service: RequestServiceOctavia, rawURL: loadBalancerClient.ResourceBaseURL()},
			serviceEndpoint{service: RequestServiceNeutron, rawURL: networkClient.ResourceBaseURL()},
		),
		observer: observer,
	}

	// Keep the process-wide limiter as the outer transport. This records only
	// requests that reach the underlying HTTP transport, and it leaves the
	// existing throttling and deadline behavior unchanged.
	if limited, ok := provider.HTTPClient.Transport.(*rateLimitedRoundTripper); ok {
		observing.next = limited.next
		limited.next = observing
		return
	}
	observing.next = provider.HTTPClient.Transport
	if observing.next == nil {
		observing.next = http.DefaultTransport
	}
	provider.HTTPClient.Transport = observing
}

type serviceEndpoint struct {
	service RequestService
	rawURL  string
}

type normalizedServiceEndpoint struct {
	service RequestService
	scheme  string
	host    string
	port    string
	path    string
}

type requestServiceClassifier struct {
	endpoints []normalizedServiceEndpoint
}

func newRequestServiceClassifier(endpoints ...serviceEndpoint) requestServiceClassifier {
	classifier := requestServiceClassifier{}
	for _, endpoint := range endpoints {
		normalized, ok := normalizeServiceEndpoint(endpoint)
		if ok {
			classifier.endpoints = append(classifier.endpoints, normalized)
		}
	}
	return classifier
}

func normalizeServiceEndpoint(endpoint serviceEndpoint) (normalizedServiceEndpoint, bool) {
	parsed, err := url.Parse(endpoint.rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" {
		return normalizedServiceEndpoint{}, false
	}
	scheme := strings.ToLower(parsed.Scheme)
	port := parsed.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	return normalizedServiceEndpoint{
		service: endpoint.service,
		scheme:  scheme,
		host:    strings.ToLower(parsed.Hostname()),
		port:    port,
		path:    normalizeURLPath(parsed.Path),
	}, true
}

func (c requestServiceClassifier) classify(request *http.Request) RequestService {
	if request == nil || request.URL == nil {
		return RequestServiceUnknown
	}
	requestEndpoint, ok := normalizeServiceEndpoint(serviceEndpoint{
		service: RequestServiceUnknown,
		rawURL:  request.URL.String(),
	})
	if !ok {
		return RequestServiceUnknown
	}

	longestPath := -1
	service := RequestServiceUnknown
	ambiguous := false
	for _, endpoint := range c.endpoints {
		if endpoint.scheme != requestEndpoint.scheme || endpoint.host != requestEndpoint.host || endpoint.port != requestEndpoint.port ||
			!endpoint.matchesPath(requestEndpoint.path) {
			continue
		}
		pathLength := len(endpoint.path)
		switch {
		case pathLength > longestPath:
			longestPath = pathLength
			service = endpoint.service
			ambiguous = false
		case pathLength == longestPath && endpoint.service != service:
			ambiguous = true
		}
	}
	if ambiguous {
		return RequestServiceUnknown
	}
	return service
}

func (e normalizedServiceEndpoint) matchesPath(value string) bool {
	// IdentityBase is commonly the origin root. Treat that root as an exact
	// match instead of claiming every path on a shared API origin as Keystone.
	// A versioned or otherwise scoped identity path remains a safe prefix.
	if e.service == RequestServiceKeystone && e.path == "/" {
		return value == e.path
	}
	return hasPathPrefix(value, e.path)
}

func normalizeURLPath(value string) string {
	return path.Clean("/" + strings.TrimPrefix(value, "/"))
}

func hasPathPrefix(value, prefix string) bool {
	return prefix == "/" || value == prefix || strings.HasPrefix(value, prefix+"/")
}

func classifyRequestMethod(request *http.Request) RequestMethod {
	if request == nil {
		return RequestMethodOther
	}
	switch strings.ToUpper(request.Method) {
	case http.MethodGet:
		return RequestMethodGet
	case http.MethodHead:
		return RequestMethodHead
	case http.MethodPost:
		return RequestMethodPost
	case http.MethodPut:
		return RequestMethodPut
	case http.MethodPatch:
		return RequestMethodPatch
	case http.MethodDelete:
		return RequestMethodDelete
	case http.MethodOptions:
		return RequestMethodOptions
	default:
		return RequestMethodOther
	}
}

func classifyStatus(response *http.Response, err error) RequestStatusClass {
	if err != nil || response == nil {
		return RequestStatusClassError
	}
	switch response.StatusCode / 100 {
	case 1:
		return RequestStatusClassInformational
	case 2:
		return RequestStatusClassSuccessful
	case 3:
		return RequestStatusClassRedirection
	case 4:
		return RequestStatusClassClientError
	case 5:
		return RequestStatusClassServerError
	default:
		return RequestStatusClassError
	}
}
