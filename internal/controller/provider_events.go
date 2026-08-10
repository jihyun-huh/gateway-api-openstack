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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
)

func conditionTransitioned(
	conditions []metav1.Condition,
	conditionType string,
	status metav1.ConditionStatus,
	reason string,
	message string,
	generation int64,
) bool {
	existing := meta.FindStatusCondition(conditions, conditionType)
	return existing == nil ||
		existing.Status != status ||
		existing.Reason != reason ||
		existing.Message != message ||
		existing.ObservedGeneration != generation
}

func conditionObservedGeneration(
	conditions []metav1.Condition,
	conditionType string,
	currentGeneration int64,
	advance bool,
) int64 {
	if advance {
		return currentGeneration
	}
	existing := meta.FindStatusCondition(conditions, conditionType)
	if existing == nil {
		return 0
	}
	return existing.ObservedGeneration
}

func recordProviderWarning(
	recorder events.EventRecorder,
	object runtime.Object,
	policy providerFailurePolicy,
	action string,
) {
	if recorder == nil || !policy.emitEvent {
		return
	}
	recorder.Eventf(object, nil, corev1.EventTypeWarning, policy.reason, action, "%s", policy.message)
}
