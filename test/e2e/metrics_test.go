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

package e2e

import (
	"net/http"
	"strings"
	"testing"
)

func TestReadMetricSnapshotRequiresEvidenceFamilies(t *testing.T) {
	body := strings.NewReader(
		mutationCounterMetricName + "{service=\"unknown\",method=\"OTHER\"} 0\n" +
			mutationInFlightMetricName + "{service=\"unknown\",method=\"OTHER\"} 0\n" +
			requestCounterMetricName + "{service=\"octavia\",method=\"GET\",status_class=\"2xx\"} 1\n" +
			requestDurationCountMetricName + "{service=\"octavia\",method=\"GET\",status_class=\"2xx\"} 1\n" +
			leaderElectionStatusMetricName + "{name=\"controller\"} 1\n" +
			processStartTimeMetricName + " 1234\n",
	)
	snapshot, err := readMetricSnapshot(body)
	if err != nil {
		t.Fatalf("readMetricSnapshot() error = %v", err)
	}
	if got := snapshot.values[leaderElectionStatusMetricName]; got != 1 {
		t.Fatalf("%s = %v, want 1", leaderElectionStatusMetricName, got)
	}
}

func TestParseMetricSnapshotAggregatesWithoutLabels(t *testing.T) {
	input := `
# HELP gateway_api_openstack_openstack_requests_total Total requests.
gateway_api_openstack_openstack_requests_total{service="octavia",method="GET",status_class="2xx"} 3 1234
gateway_api_openstack_openstack_requests_total{service="neutron",method="GET",status_class="2xx"} 4
gateway_api_openstack_openstack_mutations_total{service="octavia",method="POST"} 1
gateway_api_openstack_openstack_mutations_total{service="unknown",method="PATCH"} 2
gateway_api_openstack_openstack_mutations_in_flight{service="unknown",method="OTHER"} 0
leader_election_master_status{name="gateway-api-openstack-controller"} 1
process_start_time_seconds 1234
go_memstats_alloc_bytes 999
`
	snapshot, err := parseMetricSnapshot(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parseMetricSnapshot() error = %v", err)
	}
	if got := snapshot.values[requestCounterMetricName]; got != 7 {
		t.Fatalf("%s = %v, want 7", requestCounterMetricName, got)
	}
	if got := snapshot.values[mutationCounterMetricName]; got != 3 {
		t.Fatalf("%s = %v, want 3", mutationCounterMetricName, got)
	}
	if got := snapshot.values[mutationInFlightMetricName]; got != 0 {
		t.Fatalf("%s = %v, want 0", mutationInFlightMetricName, got)
	}
	if got := snapshot.values[leaderElectionStatusMetricName]; got != 1 {
		t.Fatalf("%s = %v, want 1", leaderElectionStatusMetricName, got)
	}
	if got := snapshot.values[processStartTimeMetricName]; got != 1234 {
		t.Fatalf("%s = %v, want 1234", processStartTimeMetricName, got)
	}
	if _, found := snapshot.values["go_memstats_alloc_bytes"]; found {
		t.Fatal("parseMetricSnapshot() retained a non-allowlisted metric")
	}
	if got := sumMetricSamples(snapshot.samples[requestCounterMetricName], map[string]string{
		"service": "octavia",
		"method":  http.MethodGet,
	}); got != 3 {
		t.Fatalf("Octavia GET requests = %v, want 3", got)
	}
}

func TestParseMetricSnapshotRejectsNonFiniteValue(t *testing.T) {
	_, err := parseMetricSnapshot(strings.NewReader(requestCounterMetricName + " NaN\n"))
	if err == nil {
		t.Fatal("parseMetricSnapshot() accepted NaN")
	}
}

func TestValidateRequiredMetricFamilies(t *testing.T) {
	complete, _ := validMetricSnapshots()
	if err := validateRequiredMetricFamilies(complete); err != nil {
		t.Fatalf("validateRequiredMetricFamilies() error = %v", err)
	}
	for name := range complete.values {
		t.Run(name, func(t *testing.T) {
			incomplete := cloneMetricsForTest(complete)
			delete(incomplete.values, name)
			delete(incomplete.samples, name)
			if err := validateRequiredMetricFamilies(incomplete); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("validateRequiredMetricFamilies() error = %v, want missing %s", err, name)
			}
		})
	}
}

func TestMetricCounterDelta(t *testing.T) {
	before := metricSnapshot{values: map[string]float64{mutationCounterMetricName: 3}}
	after := metricSnapshot{values: map[string]float64{mutationCounterMetricName: 5}}
	got, err := metricCounterDelta(before, after, mutationCounterMetricName)
	if err != nil || got != 2 {
		t.Fatalf("metricCounterDelta() = %v, %v, want 2, nil", got, err)
	}
	if _, err := metricCounterDelta(after, before, mutationCounterMetricName); err == nil {
		t.Fatal("metricCounterDelta() accepted a decreasing counter")
	}
	if _, err := metricCounterDelta(metricSnapshot{values: map[string]float64{}}, after, mutationCounterMetricName); err == nil {
		t.Fatal("metricCounterDelta() accepted a missing before counter")
	}
	if _, err := metricCounterDelta(before, metricSnapshot{values: map[string]float64{}}, mutationCounterMetricName); err == nil {
		t.Fatal("metricCounterDelta() accepted a missing after counter")
	}
}

func TestValidateConvergedNoOp(t *testing.T) {
	const lease = "gateway-api-openstack-controller"
	validBefore, validAfter := validMetricSnapshots()
	if err := validateConvergedNoOp(validBefore, validAfter, lease); err != nil {
		t.Fatalf("validateConvergedNoOp() error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*metricSnapshot, *metricSnapshot)
	}{
		{name: "follower endpoint", mutate: func(before, _ *metricSnapshot) {
			setOnlyMetricSample(before, leaderElectionStatusMetricName, 0)
		}},
		{name: "wrong Lease label", mutate: func(before, _ *metricSnapshot) {
			before.samples[leaderElectionStatusMetricName][0].labels["name"] = "another-controller"
		}},
		{name: "multiple leader samples", mutate: func(before, _ *metricSnapshot) {
			before.samples[leaderElectionStatusMetricName] = append(
				before.samples[leaderElectionStatusMetricName],
				metricSample{labels: map[string]string{"name": "another-controller"}, value: 0},
			)
		}},
		{name: "restarted process", mutate: func(_, after *metricSnapshot) {
			setOnlyMetricSample(after, processStartTimeMetricName, after.values[processStartTimeMetricName]+1)
		}},
		{name: "mutation in flight", mutate: func(_, after *metricSnapshot) {
			setOnlyMetricSample(after, mutationInFlightMetricName, 1)
		}},
		{name: "only Keystone request is fresh", mutate: func(before, after *metricSnapshot) {
			setOnlyMetricSample(after, requestCounterMetricName, before.samples[requestCounterMetricName][0].value)
			setOnlyMetricSample(after, requestDurationCountMetricName, before.samples[requestDurationCountMetricName][0].value)
			addMetricSample(after, requestCounterMetricName, map[string]string{
				"service": "keystone", "method": http.MethodPost, "status_class": "2xx",
			}, 1)
			addMetricSample(after, requestDurationCountMetricName, map[string]string{
				"service": "keystone", "method": http.MethodPost, "status_class": "2xx",
			}, 1)
		}},
		{name: "mutation attempt", mutate: func(_, after *metricSnapshot) {
			setOnlyMetricSample(after, mutationCounterMetricName, after.values[mutationCounterMetricName]+1)
		}},
		{name: "decreased counter series", mutate: func(before, after *metricSnapshot) {
			setOnlyMetricSample(after, requestDurationCountMetricName, before.values[requestDurationCountMetricName]-1)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := cloneMetricsForTest(validBefore)
			after := cloneMetricsForTest(validAfter)
			test.mutate(&before, &after)
			if err := validateConvergedNoOp(before, after, lease); err == nil {
				t.Fatal("validateConvergedNoOp() accepted invalid evidence")
			}
		})
	}
}

func TestParseMetricSnapshotRejectsAmbiguousAllowlistedSeries(t *testing.T) {
	for _, input := range []string{
		leaderElectionStatusMetricName + `{name="controller"} 1` + "\n" +
			leaderElectionStatusMetricName + `{name="controller"} 1` + "\n",
		leaderElectionStatusMetricName + `{name="controller",name="other"} 1` + "\n",
		requestCounterMetricName + `{service="octavia",method="GET" 1` + "\n",
	} {
		if _, err := parseMetricSnapshot(strings.NewReader(input)); err == nil {
			t.Fatal("parseMetricSnapshot() accepted ambiguous allowlisted series")
		}
	}
}

func validMetricSnapshots() (metricSnapshot, metricSnapshot) {
	before := newMetricSnapshotForTest()
	after := newMetricSnapshotForTest()
	labels := map[string]string{"service": "octavia", "method": http.MethodGet, "status_class": "2xx"}
	for _, item := range []struct {
		name        string
		beforeValue float64
		afterValue  float64
		labels      map[string]string
	}{
		{name: mutationCounterMetricName, beforeValue: 4, afterValue: 4, labels: map[string]string{"service": "unknown", "method": "OTHER"}},
		{name: mutationInFlightMetricName, beforeValue: 0, afterValue: 0, labels: map[string]string{"service": "unknown", "method": "OTHER"}},
		{name: requestCounterMetricName, beforeValue: 10, afterValue: 12, labels: labels},
		{name: requestDurationCountMetricName, beforeValue: 10, afterValue: 12, labels: labels},
		{name: leaderElectionStatusMetricName, beforeValue: 1, afterValue: 1, labels: map[string]string{"name": "gateway-api-openstack-controller"}},
		{name: processStartTimeMetricName, beforeValue: 1234, afterValue: 1234, labels: map[string]string{}},
	} {
		addMetricSample(&before, item.name, item.labels, item.beforeValue)
		addMetricSample(&after, item.name, item.labels, item.afterValue)
	}
	return before, after
}

func newMetricSnapshotForTest() metricSnapshot {
	return metricSnapshot{values: make(map[string]float64), samples: make(map[string][]metricSample)}
}

func addMetricSample(snapshot *metricSnapshot, name string, labels map[string]string, value float64) {
	snapshot.values[name] += value
	snapshot.samples[name] = append(snapshot.samples[name], metricSample{labels: cloneStringMap(labels), value: value})
}

func setOnlyMetricSample(snapshot *metricSnapshot, name string, value float64) {
	snapshot.values[name] = value
	snapshot.samples[name][0].value = value
}

func cloneMetricsForTest(input metricSnapshot) metricSnapshot {
	output := newMetricSnapshotForTest()
	for name, value := range input.values {
		output.values[name] = value
	}
	for name, samples := range input.samples {
		for _, sample := range samples {
			output.samples[name] = append(output.samples[name], metricSample{
				labels: cloneStringMap(sample.labels),
				value:  sample.value,
			})
		}
	}
	return output
}

func cloneStringMap(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	for name, value := range input {
		output[name] = value
	}
	return output
}
