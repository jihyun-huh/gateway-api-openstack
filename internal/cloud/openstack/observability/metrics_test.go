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

package observability

import (
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack/clients"
)

func TestRequestMetricsRecordsRequestsDurationsAndMutations(t *testing.T) {
	registry := prometheus.NewRegistry()
	requestMetrics, err := RegisterRequestMetrics(registry)
	if err != nil {
		t.Fatalf("RegisterRequestMetrics() error = %v", err)
	}
	requestMetrics.ObserveRequestStart(clients.RequestStartObservation{
		Service: clients.RequestServiceOctavia,
		Method:  clients.RequestMethodPost,
	})
	mutationLabels := map[string]string{"service": "octavia", "method": "POST"}
	if got := gatheredCounterValue(t, registry, mutationsMetricName, mutationLabels); got != 1 {
		t.Fatalf("mutation counter before completion = %v, want 1", got)
	}
	if got := gatheredGaugeValue(t, registry, inFlightMetricName, mutationLabels); got != 1 {
		t.Fatalf("in-flight mutations before completion = %v, want 1", got)
	}
	requestMetrics.ObserveRequest(clients.RequestObservation{
		Service:     clients.RequestServiceOctavia,
		Method:      clients.RequestMethodPost,
		StatusClass: clients.RequestStatusClassSuccessful,
		Duration:    250 * time.Millisecond,
	})

	labels := map[string]string{"service": "octavia", "method": "POST", "status_class": "2xx"}
	if got := gatheredCounterValue(t, registry, requestsMetricName, labels); got != 1 {
		t.Fatalf("request counter = %v, want 1", got)
	}
	if got := gatheredCounterValue(t, registry, mutationsMetricName, mutationLabels); got != 1 {
		t.Fatalf("mutation counter = %v, want 1", got)
	}
	if got := gatheredGaugeValue(t, registry, inFlightMetricName, mutationLabels); got != 0 {
		t.Fatalf("in-flight mutations after completion = %v, want 0", got)
	}

	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	foundDuration := false
	for _, family := range metricFamilies {
		if family.GetName() != durationMetricName {
			continue
		}
		foundDuration = true
		if len(family.Metric) != 1 {
			t.Fatalf("duration metric series = %d, want 1", len(family.Metric))
		}
		histogram := family.Metric[0].GetHistogram()
		if histogram == nil || histogram.GetSampleCount() != 1 || math.Abs(histogram.GetSampleSum()-0.25) > 0.000001 {
			t.Fatalf("duration histogram = %#v, want count 1 and sum 0.25", histogram)
		}
	}
	if !foundDuration {
		t.Fatalf("metric family %q was not gathered", durationMetricName)
	}
}

func TestRegisterRequestMetricsReusesCompatibleCollectors(t *testing.T) {
	registry := prometheus.NewRegistry()
	first, err := RegisterRequestMetrics(registry)
	if err != nil {
		t.Fatalf("first RegisterRequestMetrics() error = %v", err)
	}
	second, err := RegisterRequestMetrics(registry)
	if err != nil {
		t.Fatalf("second RegisterRequestMetrics() error = %v", err)
	}
	if first.requests != second.requests || first.durations != second.durations || first.mutations != second.mutations ||
		first.inFlight != second.inFlight {
		t.Fatal("RegisterRequestMetrics() did not reuse registered collectors")
	}

	observation := clients.RequestObservation{
		Service:     clients.RequestServiceNeutron,
		Method:      clients.RequestMethodGet,
		StatusClass: clients.RequestStatusClassSuccessful,
	}
	first.ObserveRequest(observation)
	second.ObserveRequest(observation)
	labels := map[string]string{"service": "neutron", "method": "GET", "status_class": "2xx"}
	if got := gatheredCounterValue(t, registry, requestsMetricName, labels); got != 2 {
		t.Fatalf("shared request counter = %v, want 2", got)
	}
}

func TestRequestMetricsBoundsUnexpectedObservationValues(t *testing.T) {
	registry := prometheus.NewRegistry()
	requestMetrics, err := RegisterRequestMetrics(registry)
	if err != nil {
		t.Fatalf("RegisterRequestMetrics() error = %v", err)
	}
	requestMetrics.ObserveRequestStart(clients.RequestStartObservation{
		Service: clients.RequestService("https://project.example.test/private-id"),
		Method:  clients.RequestMethod("CUSTOM-private-id"),
	})
	requestMetrics.ObserveRequest(clients.RequestObservation{
		Service:     clients.RequestService("https://project.example.test/private-id"),
		Method:      clients.RequestMethod("CUSTOM-private-id"),
		StatusClass: clients.RequestStatusClass("601-private-id"),
		Duration:    -time.Second,
	})

	labels := map[string]string{"service": "unknown", "method": "OTHER", "status_class": "error"}
	if got := gatheredCounterValue(t, registry, requestsMetricName, labels); got != 1 {
		t.Fatalf("bounded request counter = %v, want 1", got)
	}
	mutationLabels := map[string]string{"service": "unknown", "method": "OTHER"}
	if got := gatheredCounterValue(t, registry, mutationsMetricName, mutationLabels); got != 1 {
		t.Fatalf("bounded mutation counter = %v, want 1", got)
	}
	if got := gatheredGaugeValue(t, registry, inFlightMetricName, mutationLabels); got != 0 {
		t.Fatalf("bounded in-flight mutations = %v, want 0", got)
	}

	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	for _, family := range metricFamilies {
		if family.GetName() != durationMetricName {
			continue
		}
		for _, metric := range family.Metric {
			if histogram := metric.GetHistogram(); histogram != nil && histogram.GetSampleSum() < 0 {
				t.Fatalf("duration histogram sum = %v, want non-negative", histogram.GetSampleSum())
			}
		}
	}
}

func TestRequestMetricsDoesNotCountKeystoneAsMutation(t *testing.T) {
	registry := prometheus.NewRegistry()
	requestMetrics, err := RegisterRequestMetrics(registry)
	if err != nil {
		t.Fatalf("RegisterRequestMetrics() error = %v", err)
	}
	requestMetrics.ObserveRequestStart(clients.RequestStartObservation{
		Service: clients.RequestServiceKeystone,
		Method:  clients.RequestMethodPost,
	})
	labels := map[string]string{"service": "keystone", "method": "POST"}
	if got := gatheredCounterValue(t, registry, mutationsMetricName, labels); got != 0 {
		t.Fatalf("Keystone mutation counter = %v, want 0", got)
	}
	if got := gatheredGaugeValue(t, registry, inFlightMetricName, labels); got != 0 {
		t.Fatalf("Keystone in-flight mutations = %v, want 0", got)
	}
}

func TestRequestMetricsCountsUnknownMutation(t *testing.T) {
	registry := prometheus.NewRegistry()
	requestMetrics, err := RegisterRequestMetrics(registry)
	if err != nil {
		t.Fatalf("RegisterRequestMetrics() error = %v", err)
	}
	requestMetrics.ObserveRequestStart(clients.RequestStartObservation{
		Service: clients.RequestServiceUnknown,
		Method:  clients.RequestMethodPatch,
	})
	labels := map[string]string{"service": "unknown", "method": "PATCH"}
	if got := gatheredCounterValue(t, registry, mutationsMetricName, labels); got != 1 {
		t.Fatalf("unknown mutation counter = %v, want 1", got)
	}
	if got := gatheredGaugeValue(t, registry, inFlightMetricName, labels); got != 1 {
		t.Fatalf("unknown in-flight mutations = %v, want 1", got)
	}
	requestMetrics.ObserveRequest(clients.RequestObservation{
		Service:     clients.RequestServiceUnknown,
		Method:      clients.RequestMethodPatch,
		StatusClass: clients.RequestStatusClassError,
	})
	if got := gatheredGaugeValue(t, registry, inFlightMetricName, labels); got != 0 {
		t.Fatalf("unknown in-flight mutations after completion = %v, want 0", got)
	}
}

func TestIsBoundedMutation(t *testing.T) {
	tests := []struct {
		name    string
		service clients.RequestService
		method  clients.RequestMethod
		want    bool
	}{
		{name: "Octavia POST", service: clients.RequestServiceOctavia, method: clients.RequestMethodPost, want: true},
		{name: "Neutron DELETE", service: clients.RequestServiceNeutron, method: clients.RequestMethodDelete, want: true},
		{name: "unknown OTHER", service: clients.RequestServiceUnknown, method: clients.RequestMethodOther, want: true},
		{name: "Octavia GET", service: clients.RequestServiceOctavia, method: clients.RequestMethodGet, want: false},
		{name: "Octavia HEAD", service: clients.RequestServiceOctavia, method: clients.RequestMethodHead, want: false},
		{name: "Octavia OPTIONS", service: clients.RequestServiceOctavia, method: clients.RequestMethodOptions, want: false},
		{name: "Keystone POST", service: clients.RequestServiceKeystone, method: clients.RequestMethodPost, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isBoundedMutation(test.service, test.method); got != test.want {
				t.Fatalf("isBoundedMutation(%q, %q) = %t, want %t", test.service, test.method, got, test.want)
			}
		})
	}
}

func TestRegisterRequestMetricsPreinitializesMutationFamilies(t *testing.T) {
	registry := prometheus.NewRegistry()
	if _, err := RegisterRequestMetrics(registry); err != nil {
		t.Fatalf("RegisterRequestMetrics() error = %v", err)
	}
	labels := map[string]string{"service": "unknown", "method": "OTHER"}
	if got, found := gatheredMetricValue(t, registry, mutationsMetricName, labels, "counter"); !found || got != 0 {
		t.Fatalf("preinitialized mutation counter = %v, found=%t, want 0, true", got, found)
	}
	if got, found := gatheredMetricValue(t, registry, inFlightMetricName, labels, "gauge"); !found || got != 0 {
		t.Fatalf("preinitialized in-flight gauge = %v, found=%t, want 0, true", got, found)
	}
}

func TestRegisterRequestMetricsRequiresRegisterer(t *testing.T) {
	if _, err := RegisterRequestMetrics(nil); err == nil {
		t.Fatal("RegisterRequestMetrics() accepted a nil registerer")
	}
}

func TestNilRequestMetricsIsNoOp(t *testing.T) {
	var requestMetrics *RequestMetrics
	requestMetrics.ObserveRequestStart(clients.RequestStartObservation{})
	requestMetrics.ObserveRequest(clients.RequestObservation{})
}

func gatheredCounterValue(
	t *testing.T,
	gatherer prometheus.Gatherer,
	metricName string,
	wantLabels map[string]string,
) float64 {
	t.Helper()
	value, _ := gatheredMetricValue(t, gatherer, metricName, wantLabels, "counter")
	return value
}

func gatheredGaugeValue(
	t *testing.T,
	gatherer prometheus.Gatherer,
	metricName string,
	wantLabels map[string]string,
) float64 {
	t.Helper()
	value, _ := gatheredMetricValue(t, gatherer, metricName, wantLabels, "gauge")
	return value
}

func gatheredMetricValue(
	t *testing.T,
	gatherer prometheus.Gatherer,
	metricName string,
	wantLabels map[string]string,
	metricType string,
) (float64, bool) {
	t.Helper()
	metricFamilies, err := gatherer.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	for _, family := range metricFamilies {
		if family.GetName() != metricName {
			continue
		}
		for _, metric := range family.Metric {
			gotLabels := make(map[string]string, len(metric.Label))
			for _, pair := range metric.Label {
				gotLabels[pair.GetName()] = pair.GetValue()
			}
			matches := len(gotLabels) == len(wantLabels)
			for name, value := range wantLabels {
				matches = matches && gotLabels[name] == value
			}
			if !matches {
				continue
			}
			switch metricType {
			case "counter":
				if metric.Counter == nil {
					t.Fatalf("metric %q with labels %v is not a counter", metricName, wantLabels)
				}
				return metric.Counter.GetValue(), true
			case "gauge":
				if metric.Gauge == nil {
					t.Fatalf("metric %q with labels %v is not a gauge", metricName, wantLabels)
				}
				return metric.Gauge.GetValue(), true
			default:
				t.Fatalf("unsupported metric type %q", metricType)
			}
		}
	}
	return 0, false
}
