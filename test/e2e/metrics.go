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
	"bufio"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

const (
	maximumMetricsResponseBytes = 2 << 20

	mutationCounterMetricName      = "gateway_api_openstack_openstack_mutations_total"
	mutationInFlightMetricName     = "gateway_api_openstack_openstack_mutations_in_flight"
	requestCounterMetricName       = "gateway_api_openstack_openstack_requests_total"
	requestDurationCountMetricName = "gateway_api_openstack_openstack_request_duration_seconds_count"
	leaderElectionStatusMetricName = "leader_election_master_status"
	processStartTimeMetricName     = "process_start_time_seconds"
)

var allowedMetricFamilies = map[string]struct{}{
	mutationCounterMetricName:      {},
	mutationInFlightMetricName:     {},
	requestCounterMetricName:       {},
	requestDurationCountMetricName: {},
	leaderElectionStatusMetricName: {},
	processStartTimeMetricName:     {},
}

type metricSample struct {
	labels map[string]string
	value  float64
}

type metricSnapshot struct {
	values  map[string]float64
	samples map[string][]metricSample
}

func readMetricSnapshot(reader io.Reader) (metricSnapshot, error) {
	contents, err := io.ReadAll(io.LimitReader(reader, maximumMetricsResponseBytes+1))
	if err != nil {
		return metricSnapshot{}, fmt.Errorf("read metrics response: %w", err)
	}
	if len(contents) > maximumMetricsResponseBytes {
		return metricSnapshot{}, fmt.Errorf("metrics response exceeds the safe size limit")
	}
	snapshot, err := parseMetricSnapshot(strings.NewReader(string(contents)))
	if err != nil {
		return metricSnapshot{}, err
	}
	if err := validateRequiredMetricFamilies(snapshot); err != nil {
		return metricSnapshot{}, err
	}
	return snapshot, nil
}

func validateRequiredMetricFamilies(snapshot metricSnapshot) error {
	for _, name := range []string{
		mutationCounterMetricName,
		mutationInFlightMetricName,
		requestCounterMetricName,
		requestDurationCountMetricName,
		leaderElectionStatusMetricName,
		processStartTimeMetricName,
	} {
		if _, found := snapshot.values[name]; !found {
			return fmt.Errorf("metrics endpoint did not expose required family %s", name)
		}
	}
	return nil
}

func metricCounterDelta(before, after metricSnapshot, name string) (float64, error) {
	beforeValue, found := before.values[name]
	if !found {
		return 0, fmt.Errorf("before snapshot did not contain counter %s", name)
	}
	afterValue, found := after.values[name]
	if !found {
		return 0, fmt.Errorf("after snapshot did not contain counter %s", name)
	}
	if afterValue < beforeValue {
		return 0, fmt.Errorf("counter %s decreased during the observation window", name)
	}
	return afterValue - beforeValue, nil
}

func validateConvergedNoOp(before, after metricSnapshot, leaderLease string) error {
	if err := validateConvergedMetricSnapshots(before, after, leaderLease); err != nil {
		return err
	}
	requestDelta, durationDelta, mutationDelta, err := convergedMetricDeltas(before, after)
	if err != nil {
		return err
	}
	if requestDelta <= 0 || durationDelta <= 0 {
		return fmt.Errorf("the controller did not perform a fresh OpenStack observation")
	}
	if mutationDelta != 0 {
		return fmt.Errorf("the controller attempted an OpenStack mutation over converged input")
	}
	return nil
}

func validateConvergedMetricSnapshots(before, after metricSnapshot, leaderLease string) error {
	if err := validateRequiredMetricFamilies(before); err != nil {
		return err
	}
	if err := validateRequiredMetricFamilies(after); err != nil {
		return err
	}
	if err := validateMetricEndpointIdentity(before, leaderLease); err != nil {
		return fmt.Errorf("validate first metrics snapshot: %w", err)
	}
	if err := validateMetricEndpointIdentity(after, leaderLease); err != nil {
		return fmt.Errorf("validate second metrics snapshot: %w", err)
	}
	if before.values[processStartTimeMetricName] != after.values[processStartTimeMetricName] {
		return fmt.Errorf("metrics process changed during the observation window")
	}
	for _, name := range []string{requestCounterMetricName, requestDurationCountMetricName, mutationCounterMetricName} {
		if err := validateCounterSeriesDoNotDecrease(before, after, name); err != nil {
			return err
		}
	}
	return nil
}

func convergedMetricDeltas(before, after metricSnapshot) (float64, float64, float64, error) {
	requestDelta, err := boundedMetricDelta(before, after, requestCounterMetricName, map[string]string{
		"service": "octavia",
		"method":  http.MethodGet,
	})
	if err != nil {
		return 0, 0, 0, err
	}
	durationDelta, err := boundedMetricDelta(before, after, requestDurationCountMetricName, map[string]string{
		"service": "octavia",
		"method":  http.MethodGet,
	})
	if err != nil {
		return 0, 0, 0, err
	}
	mutationDelta, err := metricCounterDelta(before, after, mutationCounterMetricName)
	if err != nil {
		return 0, 0, 0, err
	}
	return requestDelta, durationDelta, mutationDelta, nil
}

func validateMetricEndpointIdentity(snapshot metricSnapshot, leaderLease string) error {
	leaderSamples := snapshot.samples[leaderElectionStatusMetricName]
	if len(leaderSamples) != 1 || len(leaderSamples[0].labels) != 1 || leaderSamples[0].labels["name"] != leaderLease ||
		leaderSamples[0].value != 1 {
		return fmt.Errorf("metrics endpoint did not expose exactly one current leader sample for the configured Lease")
	}
	processSamples := snapshot.samples[processStartTimeMetricName]
	if len(processSamples) != 1 || len(processSamples[0].labels) != 0 || processSamples[0].value <= 0 {
		return fmt.Errorf("metrics endpoint did not expose exactly one valid process start sample")
	}
	for _, sample := range snapshot.samples[mutationInFlightMetricName] {
		if sample.value != 0 {
			return fmt.Errorf("an OpenStack mutation was in flight at an observation boundary")
		}
	}
	return nil
}

func validateCounterSeriesDoNotDecrease(before, after metricSnapshot, name string) error {
	afterByLabels := make(map[string]float64, len(after.samples[name]))
	for _, sample := range after.samples[name] {
		afterByLabels[canonicalMetricLabels(sample.labels)] = sample.value
	}
	for _, sample := range before.samples[name] {
		afterValue, found := afterByLabels[canonicalMetricLabels(sample.labels)]
		if !found || afterValue < sample.value {
			return fmt.Errorf("counter %s decreased or lost a label set during the observation window", name)
		}
	}
	return nil
}

func boundedMetricDelta(before, after metricSnapshot, name string, labels map[string]string) (float64, error) {
	beforeValue := sumMetricSamples(before.samples[name], labels)
	afterValue := sumMetricSamples(after.samples[name], labels)
	if afterValue < beforeValue {
		return 0, fmt.Errorf("bounded counter %s decreased during the observation window", name)
	}
	return afterValue - beforeValue, nil
}

func sumMetricSamples(samples []metricSample, labels map[string]string) float64 {
	var total float64
	for _, sample := range samples {
		matches := true
		for name, expected := range labels {
			if sample.labels[name] != expected {
				matches = false
				break
			}
		}
		if matches {
			total += sample.value
		}
	}
	return total
}

func parseMetricSnapshot(reader io.Reader) (metricSnapshot, error) {
	snapshot := metricSnapshot{
		values:  make(map[string]float64),
		samples: make(map[string][]metricSample),
	}
	seen := make(map[string]map[string]struct{})
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		name, sample, found, err := parseAllowlistedMetricSample(scanner.Text())
		if err != nil {
			return metricSnapshot{}, err
		}
		if !found {
			continue
		}
		if err := recordMetricSample(&snapshot, seen, name, sample); err != nil {
			return metricSnapshot{}, err
		}
	}
	if err := scanner.Err(); err != nil {
		return metricSnapshot{}, fmt.Errorf("read metrics response: %w", err)
	}
	return snapshot, nil
}

func parseAllowlistedMetricSample(line string) (string, metricSample, bool, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", metricSample{}, false, nil
	}
	name, labels, valueText, found, err := metricNameLabelsAndValue(line)
	if !found {
		return "", metricSample{}, false, nil
	}
	if _, allowed := allowedMetricFamilies[name]; !allowed {
		return "", metricSample{}, false, nil
	}
	if err != nil {
		return "", metricSample{}, false, fmt.Errorf("parse allowlisted metric %s: %w", name, err)
	}
	value, err := strconv.ParseFloat(valueText, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return "", metricSample{}, false, fmt.Errorf("allowlisted metric %s has a non-finite value", name)
	}
	if value < 0 {
		return "", metricSample{}, false, fmt.Errorf("allowlisted metric %s has a negative value", name)
	}
	return name, metricSample{labels: labels, value: value}, true, nil
}

func recordMetricSample(snapshot *metricSnapshot, seen map[string]map[string]struct{}, name string, sample metricSample) error {
	labelKey := canonicalMetricLabels(sample.labels)
	if seen[name] == nil {
		seen[name] = make(map[string]struct{})
	}
	if _, duplicate := seen[name][labelKey]; duplicate {
		return fmt.Errorf("allowlisted metric %s repeats a label set", name)
	}
	seen[name][labelKey] = struct{}{}
	snapshot.values[name] += sample.value
	snapshot.samples[name] = append(snapshot.samples[name], sample)
	return nil
}

func metricNameLabelsAndValue(line string) (string, map[string]string, string, bool, error) {
	nameEnd := strings.IndexAny(line, "{ \t")
	if nameEnd <= 0 {
		return "", nil, "", false, nil
	}
	name := line[:nameEnd]
	remainder := line[nameEnd:]
	labels := map[string]string{}
	if remainder[0] == '{' {
		labelsEnd := strings.LastIndexByte(remainder, '}')
		if labelsEnd < 0 {
			return name, nil, "", true, fmt.Errorf("metric labels are not terminated")
		}
		var err error
		labels, err = parseMetricLabels(remainder[1:labelsEnd])
		if err != nil {
			return name, nil, "", true, err
		}
		remainder = remainder[labelsEnd+1:]
	}
	fields := strings.Fields(remainder)
	if len(fields) == 0 {
		return name, nil, "", true, fmt.Errorf("metric value is absent")
	}
	return name, labels, fields[0], true, nil
}

func parseMetricLabels(input string) (map[string]string, error) {
	labels := make(map[string]string)
	input = strings.TrimSpace(input)
	for input != "" {
		name, value, remainder, err := parseMetricLabel(input)
		if err != nil {
			return nil, err
		}
		if _, duplicate := labels[name]; duplicate {
			return nil, fmt.Errorf("metric label %q is repeated", name)
		}
		labels[name] = value
		input, err = nextMetricLabelInput(remainder)
		if err != nil {
			return nil, err
		}
	}
	return labels, nil
}

func parseMetricLabel(input string) (string, string, string, error) {
	equals := strings.IndexByte(input, '=')
	if equals <= 0 {
		return "", "", "", fmt.Errorf("metric label is missing an equals sign")
	}
	name := strings.TrimSpace(input[:equals])
	remainder := strings.TrimSpace(input[equals+1:])
	if name == "" || !strings.HasPrefix(remainder, `"`) {
		return "", "", "", fmt.Errorf("metric label has an invalid name or value")
	}
	end := quotedMetricLabelEnd(remainder)
	if end < 0 {
		return "", "", "", fmt.Errorf("metric label value is not terminated")
	}
	value, err := strconv.Unquote(remainder[:end+1])
	if err != nil {
		return "", "", "", fmt.Errorf("unquote metric label: %w", err)
	}
	return name, value, strings.TrimSpace(remainder[end+1:]), nil
}

func nextMetricLabelInput(remainder string) (string, error) {
	if remainder == "" {
		return "", nil
	}
	if remainder[0] != ',' {
		return "", fmt.Errorf("metric labels are not comma separated")
	}
	input := strings.TrimSpace(remainder[1:])
	if input == "" {
		return "", fmt.Errorf("metric labels end with a comma")
	}
	return input, nil
}

func quotedMetricLabelEnd(value string) int {
	escaped := false
	for index := 1; index < len(value); index++ {
		switch {
		case escaped:
			escaped = false
		case value[index] == '\\':
			escaped = true
		case value[index] == '"':
			return index
		}
	}
	return -1
}

func canonicalMetricLabels(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for name := range labels {
		names = append(names, name)
	}
	slices.Sort(names)
	var output strings.Builder
	for _, name := range names {
		output.WriteString(strconv.Quote(name))
		output.WriteByte('=')
		output.WriteString(strconv.Quote(labels[name]))
		output.WriteByte(',')
	}
	return output.String()
}
