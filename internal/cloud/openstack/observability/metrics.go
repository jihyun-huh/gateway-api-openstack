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

// Package observability provides controller process metrics for the OpenStack
// adapter without coupling the adapter to controller-runtime.
package observability

import (
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack/clients"
)

const (
	metricsNamespace = "gateway_api_openstack"
	metricsSubsystem = "openstack"

	requestsMetricName  = "gateway_api_openstack_openstack_requests_total"
	durationMetricName  = "gateway_api_openstack_openstack_request_duration_seconds"
	mutationsMetricName = "gateway_api_openstack_openstack_mutations_total"
	inFlightMetricName  = "gateway_api_openstack_openstack_mutations_in_flight"
)

// RequestMetrics records bounded OpenStack request classifications. It
// implements clients.RequestObserver.
type RequestMetrics struct {
	requests  *prometheus.CounterVec
	durations *prometheus.HistogramVec
	mutations *prometheus.CounterVec
	inFlight  *prometheus.GaugeVec
}

// RegisterRequestMetrics registers OpenStack request metrics with registerer.
// Existing compatible collectors are reused so repeated manager setup in the
// same process is safe.
func RegisterRequestMetrics(registerer prometheus.Registerer) (*RequestMetrics, error) {
	if registerer == nil {
		return nil, fmt.Errorf("metrics registerer is required")
	}

	requests, err := registerCounterVec(registerer, prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "requests_total",
			Help:      "Total completed OpenStack API requests sent after service discovery.",
		},
		[]string{"service", "method", "status_class"},
	))
	if err != nil {
		return nil, fmt.Errorf("register %s: %w", requestsMetricName, err)
	}
	durations, err := registerHistogramVec(registerer, prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "request_duration_seconds",
			Help:      "OpenStack API request duration in seconds, excluding local rate limit wait.",
			Buckets: []float64{
				0.005,
				0.01,
				0.025,
				0.05,
				0.1,
				0.25,
				0.5,
				1,
				2.5,
				5,
				10,
				30,
				60,
				120,
			},
		},
		[]string{"service", "method", "status_class"},
	))
	if err != nil {
		return nil, fmt.Errorf("register %s: %w", durationMetricName, err)
	}
	mutations, err := registerCounterVec(registerer, prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "mutations_total",
			Help:      "Total potentially mutating OpenStack API request attempts started after service discovery; GET, HEAD, OPTIONS, and positively classified Keystone requests are excluded.",
		},
		[]string{"service", "method"},
	))
	if err != nil {
		return nil, fmt.Errorf("register %s: %w", mutationsMetricName, err)
	}
	inFlight, err := registerGaugeVec(registerer, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "mutations_in_flight",
			Help:      "Potentially mutating OpenStack API request attempts that have started but whose transport call has not completed.",
		},
		[]string{"service", "method"},
	))
	if err != nil {
		return nil, fmt.Errorf("register %s: %w", inFlightMetricName, err)
	}

	// Prometheus vector families do not exist until their first label set is
	// used. Initialize one bounded zero series so zero-mutation evidence is
	// observable before the first mutating request. Add(0) also preserves a
	// live gauge when compatible collectors are reused.
	mutations.WithLabelValues(string(clients.RequestServiceUnknown), string(clients.RequestMethodOther)).Add(0)
	inFlight.WithLabelValues(string(clients.RequestServiceUnknown), string(clients.RequestMethodOther)).Add(0)

	return &RequestMetrics{
		requests:  requests,
		durations: durations,
		mutations: mutations,
		inFlight:  inFlight,
	}, nil
}

// ObserveRequestStart records a request immediately before its underlying
// transport call starts.
func (m *RequestMetrics) ObserveRequestStart(observation clients.RequestStartObservation) {
	if m == nil {
		return
	}
	service := boundedService(observation.Service)
	method := boundedMethod(observation.Method)
	if !isBoundedMutation(service, method) {
		return
	}
	labels := []string{string(service), string(method)}
	// Raise the in-flight guard first. A scrape between these two operations is
	// then conservatively invalid instead of appearing to prove a quiet window.
	m.inFlight.WithLabelValues(labels...).Inc()
	m.mutations.WithLabelValues(labels...).Inc()
}

// ObserveRequest records an OpenStack request using only bounded label values.
func (m *RequestMetrics) ObserveRequest(observation clients.RequestObservation) {
	if m == nil {
		return
	}
	service := boundedService(observation.Service)
	method := boundedMethod(observation.Method)
	statusClass := boundedStatusClass(observation.StatusClass)
	if isBoundedMutation(service, method) {
		m.inFlight.WithLabelValues(string(service), string(method)).Dec()
	}
	labels := []string{string(service), string(method), string(statusClass)}

	m.requests.WithLabelValues(labels...).Inc()
	duration := observation.Duration
	if duration < 0 {
		duration = 0
	}
	m.durations.WithLabelValues(labels...).Observe(duration.Seconds())
}

func registerCounterVec(registerer prometheus.Registerer, collector *prometheus.CounterVec) (*prometheus.CounterVec, error) {
	if err := registerer.Register(collector); err != nil {
		var alreadyRegistered prometheus.AlreadyRegisteredError
		if !errors.As(err, &alreadyRegistered) {
			return nil, err
		}
		existing, ok := alreadyRegistered.ExistingCollector.(*prometheus.CounterVec)
		if !ok {
			return nil, fmt.Errorf("existing collector has type %T, want *prometheus.CounterVec", alreadyRegistered.ExistingCollector)
		}
		return existing, nil
	}
	return collector, nil
}

func registerHistogramVec(registerer prometheus.Registerer, collector *prometheus.HistogramVec) (*prometheus.HistogramVec, error) {
	if err := registerer.Register(collector); err != nil {
		var alreadyRegistered prometheus.AlreadyRegisteredError
		if !errors.As(err, &alreadyRegistered) {
			return nil, err
		}
		existing, ok := alreadyRegistered.ExistingCollector.(*prometheus.HistogramVec)
		if !ok {
			return nil, fmt.Errorf("existing collector has type %T, want *prometheus.HistogramVec", alreadyRegistered.ExistingCollector)
		}
		return existing, nil
	}
	return collector, nil
}

func registerGaugeVec(registerer prometheus.Registerer, collector *prometheus.GaugeVec) (*prometheus.GaugeVec, error) {
	if err := registerer.Register(collector); err != nil {
		var alreadyRegistered prometheus.AlreadyRegisteredError
		if !errors.As(err, &alreadyRegistered) {
			return nil, err
		}
		existing, ok := alreadyRegistered.ExistingCollector.(*prometheus.GaugeVec)
		if !ok {
			return nil, fmt.Errorf("existing collector has type %T, want *prometheus.GaugeVec", alreadyRegistered.ExistingCollector)
		}
		return existing, nil
	}
	return collector, nil
}

func boundedService(service clients.RequestService) clients.RequestService {
	switch service {
	case clients.RequestServiceKeystone, clients.RequestServiceOctavia, clients.RequestServiceNeutron:
		return service
	default:
		return clients.RequestServiceUnknown
	}
}

func boundedMethod(method clients.RequestMethod) clients.RequestMethod {
	switch method {
	case clients.RequestMethodGet,
		clients.RequestMethodHead,
		clients.RequestMethodPost,
		clients.RequestMethodPut,
		clients.RequestMethodPatch,
		clients.RequestMethodDelete,
		clients.RequestMethodOptions:
		return method
	default:
		return clients.RequestMethodOther
	}
}

func boundedStatusClass(statusClass clients.RequestStatusClass) clients.RequestStatusClass {
	switch statusClass {
	case clients.RequestStatusClassInformational,
		clients.RequestStatusClassSuccessful,
		clients.RequestStatusClassRedirection,
		clients.RequestStatusClassClientError,
		clients.RequestStatusClassServerError:
		return statusClass
	default:
		return clients.RequestStatusClassError
	}
}

func isBoundedMutation(service clients.RequestService, method clients.RequestMethod) bool {
	if service == clients.RequestServiceKeystone {
		return false
	}
	switch method {
	case clients.RequestMethodGet, clients.RequestMethodHead, clients.RequestMethodOptions:
		return false
	default:
		return true
	}
}

var _ clients.RequestObserver = (*RequestMetrics)(nil)
