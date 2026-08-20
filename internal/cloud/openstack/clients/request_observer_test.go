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
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"golang.org/x/time/rate"
)

func TestRequestServiceClassifierUsesLongestPathPrefix(t *testing.T) {
	classifier := newRequestServiceClassifier(
		serviceEndpoint{service: RequestServiceKeystone, rawURL: "https://api.openstack.test/"},
		serviceEndpoint{service: RequestServiceKeystone, rawURL: "https://api.openstack.test/identity/v3/"},
		serviceEndpoint{service: RequestServiceOctavia, rawURL: "https://api.openstack.test/load-balancer/v2.0/"},
		serviceEndpoint{service: RequestServiceNeutron, rawURL: "https://api.openstack.test/network/v2.0/"},
	)
	tests := []struct {
		name       string
		requestURL string
		want       RequestService
	}{
		{name: "Octavia", requestURL: "https://api.openstack.test/load-balancer/v2.0/lbaas/loadbalancers/id", want: RequestServiceOctavia},
		{name: "Neutron", requestURL: "https://api.openstack.test/network/v2.0/floatingips/id", want: RequestServiceNeutron},
		{name: "Keystone endpoint", requestURL: "https://api.openstack.test/identity/v3/auth/tokens", want: RequestServiceKeystone},
		{name: "Keystone base", requestURL: "https://api.openstack.test/", want: RequestServiceKeystone},
		{name: "unclassified shared origin path", requestURL: "https://api.openstack.test/versions", want: RequestServiceUnknown},
		{name: "different host", requestURL: "https://other.openstack.test/load-balancer/v2.0/lbaas/loadbalancers/id", want: RequestServiceUnknown},
		{name: "different scheme", requestURL: "http://api.openstack.test/load-balancer/v2.0/lbaas/loadbalancers/id", want: RequestServiceUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, test.requestURL, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := classifier.classify(request); got != test.want {
				t.Fatalf("classify() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRequestServiceClassifierRequiresPathBoundary(t *testing.T) {
	classifier := newRequestServiceClassifier(
		serviceEndpoint{service: RequestServiceOctavia, rawURL: "https://api.openstack.test/v2.0/"},
	)
	request, err := http.NewRequest(http.MethodGet, "https://api.openstack.test/v2.01/lbaas", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := classifier.classify(request); got != RequestServiceUnknown {
		t.Fatalf("classify() = %q, want unknown for a partial path-segment match", got)
	}
}

func TestRequestServiceClassifierNormalizesDefaultPort(t *testing.T) {
	classifier := newRequestServiceClassifier(
		serviceEndpoint{service: RequestServiceNeutron, rawURL: "https://api.openstack.test:443/v2.0/"},
	)
	request, err := http.NewRequest(http.MethodGet, "https://api.openstack.test/v2.0/floatingips", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := classifier.classify(request); got != RequestServiceNeutron {
		t.Fatalf("classify() = %q, want neutron", got)
	}
}

func TestRequestServiceClassifierRejectsAmbiguousLongestMatch(t *testing.T) {
	classifier := newRequestServiceClassifier(
		serviceEndpoint{service: RequestServiceOctavia, rawURL: "https://api.openstack.test/v2.0/"},
		serviceEndpoint{service: RequestServiceNeutron, rawURL: "https://api.openstack.test/v2.0/"},
	)
	request, err := http.NewRequest(http.MethodGet, "https://api.openstack.test/v2.0/resources", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := classifier.classify(request); got != RequestServiceUnknown {
		t.Fatalf("classify() = %q, want unknown for ambiguous service endpoints", got)
	}
}

func TestRequestObservingRoundTripperBoundsObservation(t *testing.T) {
	observer := &recordingRequestObserver{}
	response := &http.Response{StatusCode: http.StatusAccepted, Header: http.Header{}, Body: http.NoBody}
	transport := &requestObservingRoundTripper{
		next: testRoundTripper(func(*http.Request) (*http.Response, error) {
			if starts := observer.startSnapshot(); len(starts) != 1 {
				t.Fatalf("start observations before RoundTrip = %d, want 1", len(starts))
			}
			if observations := observer.snapshot(); len(observations) != 0 {
				t.Fatalf("completion observations before RoundTrip = %d, want 0", len(observations))
			}
			return response, nil
		}),
		classifier: newRequestServiceClassifier(
			serviceEndpoint{service: RequestServiceOctavia, rawURL: "https://api.openstack.test/v2.0/"},
		),
		observer: observer,
	}
	request, err := http.NewRequest("pOsT", "https://api.openstack.test/v2.0/lbaas/loadbalancers/private-id?token=secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	gotResponse, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	if gotResponse != response {
		t.Fatal("RoundTrip() did not return the wrapped transport response")
	}

	starts := observer.startSnapshot()
	if len(starts) != 1 || starts[0].Service != RequestServiceOctavia || starts[0].Method != RequestMethodPost {
		t.Fatalf("start observations = %#v, want bounded Octavia POST", starts)
	}
	observations := observer.snapshot()
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want 1", len(observations))
	}
	want := RequestObservation{
		Service:     RequestServiceOctavia,
		Method:      RequestMethodPost,
		StatusClass: RequestStatusClassSuccessful,
	}
	if observations[0].Service != want.Service || observations[0].Method != want.Method ||
		observations[0].StatusClass != want.StatusClass {
		t.Fatalf("observation = %#v, want bounded fields %#v", observations[0], want)
	}
	if observations[0].Duration < 0 {
		t.Fatalf("observation duration = %s, want non-negative", observations[0].Duration)
	}
}

func TestRequestObservingRoundTripperCountsUnclassifiedModifyingRequest(t *testing.T) {
	observer := &recordingRequestObserver{}
	transport := &requestObservingRoundTripper{
		next: testRoundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: http.NoBody}, nil
		}),
		classifier: newRequestServiceClassifier(
			serviceEndpoint{service: RequestServiceKeystone, rawURL: "https://api.openstack.test/"},
			serviceEndpoint{service: RequestServiceOctavia, rawURL: "https://api.openstack.test/load-balancer/v2.0/"},
		),
		observer: observer,
	}
	request, err := http.NewRequest(http.MethodPost, "https://api.openstack.test/catalog-path-mismatch/resource", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.RoundTrip(request); err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	starts := observer.startSnapshot()
	if len(starts) != 1 {
		t.Fatalf("start observations = %d, want 1", len(starts))
	}
	if got := starts[0]; got.Service != RequestServiceUnknown || got.Method != RequestMethodPost {
		t.Fatalf("start observation = %#v, want unknown POST", got)
	}
}

func TestRequestObservingRoundTripperClassifiesNilAndErrorResponses(t *testing.T) {
	testError := errors.New("transport failed")
	tests := []struct {
		name    string
		err     error
		wantErr error
	}{
		{name: "nil response", err: nil, wantErr: nil},
		{name: "transport error", err: testError, wantErr: testError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observer := &recordingRequestObserver{}
			transport := &requestObservingRoundTripper{
				next: testRoundTripper(func(*http.Request) (*http.Response, error) {
					return nil, test.err
				}),
				classifier: requestServiceClassifier{},
				observer:   observer,
			}
			request, err := http.NewRequest("CUSTOM", "https://unknown.openstack.test/resource", nil)
			if err != nil {
				t.Fatal(err)
			}
			response, gotErr := transport.RoundTrip(request)
			if response != nil || !errors.Is(gotErr, test.wantErr) {
				t.Fatalf("RoundTrip() = %#v, %v, want nil, %v", response, gotErr, test.wantErr)
			}
			observations := observer.snapshot()
			if len(observations) != 1 {
				t.Fatalf("observations = %d, want 1", len(observations))
			}
			starts := observer.startSnapshot()
			if len(starts) != 1 || starts[0].Service != RequestServiceUnknown || starts[0].Method != RequestMethodOther {
				t.Fatalf("start observations = %#v, want unknown OTHER", starts)
			}
			got := observations[0]
			if got.Service != RequestServiceUnknown || got.Method != RequestMethodOther ||
				got.StatusClass != RequestStatusClassError {
				t.Fatalf("observation = %#v, want unknown/OTHER/error", got)
			}
		})
	}
}

func TestClassifyStatus(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		err        error
		want       RequestStatusClass
	}{
		{name: "informational", statusCode: 101, want: RequestStatusClassInformational},
		{name: "successful", statusCode: 204, want: RequestStatusClassSuccessful},
		{name: "redirection", statusCode: 304, want: RequestStatusClassRedirection},
		{name: "client error", statusCode: 429, want: RequestStatusClassClientError},
		{name: "server error", statusCode: 503, want: RequestStatusClassServerError},
		{name: "invalid status", statusCode: 601, want: RequestStatusClassError},
		{name: "transport error takes precedence", statusCode: 200, err: errors.New("request failed"), want: RequestStatusClassError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{StatusCode: test.statusCode}
			if got := classifyStatus(response, test.err); got != test.want {
				t.Fatalf("classifyStatus() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestInstallRequestObserverKeepsRateLimiterOutsideObservation(t *testing.T) {
	requestCount := 0
	wrapped := testRoundTripper(func(*http.Request) (*http.Response, error) {
		requestCount++
		return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}, Body: http.NoBody}, nil
	})
	limited := &rateLimitedRoundTripper{
		next:    wrapped,
		limiter: rate.NewLimiter(0, 1),
	}
	provider := &gophercloud.ProviderClient{IdentityBase: "https://api.openstack.test/identity/"}
	provider.HTTPClient.Transport = limited
	loadBalancerClient := &gophercloud.ServiceClient{
		ProviderClient: provider,
		ResourceBase:   "https://api.openstack.test/load-balancer/v2.0/",
	}
	networkClient := &gophercloud.ServiceClient{
		ProviderClient: provider,
		ResourceBase:   "https://api.openstack.test/network/v2.0/",
	}
	observer := &recordingRequestObserver{}

	installRequestObserver(provider, loadBalancerClient, networkClient, observer)
	if provider.HTTPClient.Transport != limited {
		t.Fatalf("provider transport = %T, want original rate limiter", provider.HTTPClient.Transport)
	}
	observing, ok := limited.next.(*requestObservingRoundTripper)
	if !ok {
		t.Fatalf("rate limiter wrapped transport = %T, want requestObservingRoundTripper", limited.next)
	}
	if _, ok := observing.next.(testRoundTripper); !ok {
		t.Fatalf("observer wrapped transport = %T, want testRoundTripper", observing.next)
	}

	first, err := http.NewRequest(http.MethodDelete, "https://api.openstack.test/load-balancer/v2.0/lbaas/loadbalancers/id", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.HTTPClient.Transport.RoundTrip(first); err != nil {
		t.Fatalf("first RoundTrip() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	second, err := http.NewRequestWithContext(ctx, http.MethodDelete, "https://api.openstack.test/load-balancer/v2.0/lbaas/loadbalancers/id", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.HTTPClient.Transport.RoundTrip(second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second RoundTrip() error = %v, want context deadline exceeded", err)
	}
	if requestCount != 1 {
		t.Fatalf("wrapped transport requests = %d, want 1", requestCount)
	}
	observations := observer.snapshot()
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want only the request admitted by the limiter", len(observations))
	}
	starts := observer.startSnapshot()
	if len(starts) != 1 || starts[0].Service != RequestServiceOctavia || starts[0].Method != RequestMethodDelete {
		t.Fatalf("start observations = %#v, want one Octavia DELETE mutation", starts)
	}
}

type recordingRequestObserver struct {
	mu           sync.Mutex
	starts       []RequestStartObservation
	observations []RequestObservation
}

func (o *recordingRequestObserver) ObserveRequestStart(observation RequestStartObservation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.starts = append(o.starts, observation)
}

func (o *recordingRequestObserver) ObserveRequest(observation RequestObservation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.observations = append(o.observations, observation)
}

func (o *recordingRequestObserver) snapshot() []RequestObservation {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]RequestObservation(nil), o.observations...)
}

func (o *recordingRequestObserver) startSnapshot() []RequestStartObservation {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]RequestStartObservation(nil), o.starts...)
}
