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

package gateway

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller/graph"
)

func TestGatewayProviderFailureCategories(t *testing.T) {
	tests := []struct {
		name            string
		category        cloud.ErrorCategory
		wantStatus      metav1.ConditionStatus
		wantReason      string
		wantError       bool
		wantRequeueBase time.Duration
		wantEvent       bool
	}{
		{name: "authentication", category: cloud.ErrorCategoryAuthentication, wantStatus: metav1.ConditionUnknown, wantReason: string(gatewayv1.GatewayReasonPending), wantRequeueBase: providerFailureRequeue, wantEvent: true},
		{name: "authorization", category: cloud.ErrorCategoryAuthorization, wantStatus: metav1.ConditionUnknown, wantReason: string(gatewayv1.GatewayReasonPending), wantRequeueBase: providerFailureRequeue, wantEvent: true},
		{name: "quota", category: cloud.ErrorCategoryQuota, wantStatus: metav1.ConditionFalse, wantReason: string(gatewayv1.GatewayReasonNoResources), wantRequeueBase: providerFailureRequeue, wantEvent: true},
		{name: "rate limit", category: cloud.ErrorCategoryRateLimit, wantStatus: metav1.ConditionUnknown, wantReason: string(gatewayv1.GatewayReasonPending), wantError: true},
		{name: "timeout", category: cloud.ErrorCategoryTimeout, wantStatus: metav1.ConditionUnknown, wantReason: string(gatewayv1.GatewayReasonPending), wantError: true},
		{name: "retryable service", category: cloud.ErrorCategoryRetryableService, wantStatus: metav1.ConditionUnknown, wantReason: string(gatewayv1.GatewayReasonPending), wantError: true},
		{name: "terminal validation", category: cloud.ErrorCategoryTerminalValidation, wantStatus: metav1.ConditionFalse, wantReason: string(gatewayv1.GatewayReasonInvalid), wantEvent: true},
		{name: "resource failure", category: cloud.ErrorCategoryResourceFailure, wantStatus: metav1.ConditionFalse, wantReason: "ResourceFailed", wantRequeueBase: providerFailureRequeue, wantEvent: true},
		{name: "ownership conflict", category: cloud.ErrorCategoryOwnershipConflict, wantStatus: metav1.ConditionFalse, wantReason: "OwnershipConflict", wantRequeueBase: ownershipConflictRequeue, wantEvent: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig()
			scheme := testScheme(t)
			class := &gatewayv1.GatewayClass{
				ObjectMeta: metav1.ObjectMeta{Name: "openstack"},
				Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
			}
			gateway := programmedTestGateway(cfg)
			kubeClient := indexedFakeClientBuilder(scheme, cfg).
				WithStatusSubresource(gateway).
				WithObjects(class, gateway).
				Build()
			cause := errors.New("token=secret-token project=private-project response=private-body")
			providerErr := cloud.NewProviderError(test.category, cause)
			provider := &recordingProvider{gatewayErr: providerErr}
			recorder := events.NewFakeRecorder(2)
			reconciler := Reconciler{
				Client: kubeClient, APIReader: kubeClient, Provider: provider,
				Coordinator: &graph.Coordinator{}, Recorder: recorder, Config: cfg,
			}

			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}})
			if test.wantError {
				if !errors.Is(err, providerErr) || err.Error() == providerErr.Error() || strings.Contains(err.Error(), "secret-token") {
					t.Fatalf("Reconcile() error = %v", err)
				}
			} else if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			assertJitteredRequeue(t, result.RequeueAfter, test.wantRequeueBase)

			current := &gatewayv1.Gateway{}
			if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}, current); err != nil {
				t.Fatal(err)
			}
			programmed := meta.FindStatusCondition(current.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
			if programmed == nil || programmed.Status != test.wantStatus || programmed.Reason != test.wantReason || strings.Contains(programmed.Message, "secret-token") {
				t.Fatalf("Programmed condition = %#v", programmed)
			}
			policy, _ := controller.ProviderFailurePolicyFor(providerErr)
			if programmed.Message != policy.Message {
				t.Fatalf("Programmed message = %q, want %q", programmed.Message, policy.Message)
			}
			if test.wantEvent {
				event := receiveEvent(t, recorder)
				if !strings.Contains(event, policy.Reason) || !strings.Contains(event, policy.Message) || strings.Contains(event, "secret-token") {
					t.Fatalf("Event = %q", event)
				}
			} else {
				assertNoEvent(t, recorder)
			}
		})
	}
}

func TestGatewayProviderEventsFollowConditionTransitions(t *testing.T) {
	cfg := testConfig()
	scheme := testScheme(t)
	class := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	gateway := programmedTestGateway(cfg)
	kubeClient := indexedFakeClientBuilder(scheme, cfg).
		WithStatusSubresource(gateway).
		WithObjects(class, gateway).
		Build()
	provider := &recordingProvider{
		gatewayErr: cloud.NewProviderError(cloud.ErrorCategoryAuthentication, errors.New("secret-token")),
	}
	recorder := events.NewFakeRecorder(3)
	reconciler := Reconciler{
		Client: kubeClient, APIReader: kubeClient, Provider: provider,
		Coordinator: &graph.Coordinator{}, Recorder: recorder, Config: cfg,
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if event := receiveEvent(t, recorder); !strings.Contains(event, "AuthenticationFailed") {
		t.Fatalf("first Event = %q", event)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	assertNoEvent(t, recorder)

	provider.gatewayErr = cloud.NewProviderError(cloud.ErrorCategoryAuthorization, errors.New("different-secret-token"))
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("third Reconcile() error = %v", err)
	}
	if event := receiveEvent(t, recorder); !strings.Contains(event, "AuthorizationFailed") || strings.Contains(event, "different-secret-token") {
		t.Fatalf("third Event = %q", event)
	}

	current := &gatewayv1.Gateway{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), current); err != nil {
		t.Fatal(err)
	}
	scope := &gatewayScope{gateway: current, original: current.DeepCopy()}
	if _, err := reconciler.handleGatewayDeleteFailure(scope, provider.gatewayErr); err != nil {
		t.Fatalf("handleGatewayDeleteFailure() error = %v", err)
	}
	if err := reconciler.patchGateway(context.Background(), scope); err != nil {
		t.Fatalf("patchGateway() error = %v", err)
	}
	if event := receiveEvent(t, recorder); !strings.Contains(event, "AuthorizationFailed") || !strings.Contains(event, "Gateway cleanup") {
		t.Fatalf("cleanup Event = %q", event)
	}
	current = &gatewayv1.Gateway{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), current); err != nil {
		t.Fatal(err)
	}
	scope = &gatewayScope{gateway: current, original: current.DeepCopy()}
	if _, err := reconciler.handleGatewayDeleteFailure(scope, provider.gatewayErr); err != nil {
		t.Fatalf("second handleGatewayDeleteFailure() error = %v", err)
	}
	if err := reconciler.patchGateway(context.Background(), scope); err != nil {
		t.Fatalf("second patchGateway() error = %v", err)
	}
	assertNoEvent(t, recorder)
}

func TestGatewayProviderFailurePreservesProviderAndStatusErrors(t *testing.T) {
	cfg := testConfig()
	class := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	gateway := programmedTestGateway(cfg)
	baseClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(gateway).
		WithObjects(class, gateway).
		Build()
	statusErr := errors.New("forced status patch failure")
	kubeClient := &patchFailingStatusClient{Client: baseClient, patchErr: statusErr}
	providerErr := cloud.NewProviderError(cloud.ErrorCategoryRetryableService, errors.New("token=secret-token"))
	recorder := events.NewFakeRecorder(1)
	reconciler := Reconciler{
		Client: kubeClient, APIReader: kubeClient,
		Provider:    &recordingProvider{gatewayErr: providerErr},
		Coordinator: &graph.Coordinator{}, Recorder: recorder, Config: cfg,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gateway)})
	if !errors.Is(err, providerErr) || !errors.Is(err, statusErr) {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("Reconcile() exposed provider cause: %q", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("Reconcile() result = %#v, want zero result after patch failure", result)
	}
	assertNoEvent(t, recorder)
}

func TestGatewayWarningIsRecordedAfterStatusPatch(t *testing.T) {
	cfg := testConfig()
	class := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	gateway := programmedTestGateway(cfg)
	baseClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(gateway).
		WithObjects(class, gateway).
		Build()
	statusErr := errors.New("forced status patch failure")
	kubeClient := &patchFailingStatusClient{Client: baseClient, patchErr: statusErr}
	providerErr := cloud.NewProviderError(cloud.ErrorCategoryAuthentication, errors.New("token=secret-token"))
	recorder := events.NewFakeRecorder(2)
	reconciler := Reconciler{
		Client: kubeClient, APIReader: kubeClient,
		Provider:    &recordingProvider{gatewayErr: providerErr},
		Coordinator: &graph.Coordinator{}, Recorder: recorder, Config: cfg,
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gateway)}

	result, err := reconciler.Reconcile(context.Background(), request)
	if !errors.Is(err, providerErr) || !errors.Is(err, statusErr) {
		t.Fatalf("first Reconcile() error = %v, want provider and status patch failures", err)
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("first Reconcile() exposed provider cause: %q", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("first Reconcile() result = %#v, want zero result", result)
	}
	assertNoEvent(t, recorder)

	kubeClient.patchErr = nil
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if event := receiveEvent(t, recorder); !strings.Contains(event, "AuthenticationFailed") {
		t.Fatalf("Event = %q", event)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("third Reconcile() error = %v", err)
	}
	assertNoEvent(t, recorder)
}

func TestGatewayListenerReplacementWaitsForStatusCheckpoint(t *testing.T) {
	cfg := testConfig()
	class := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	gateway := programmedTestGateway(cfg)
	gateway.Spec.Listeners[0].Port = 8080
	baseClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(gateway).
		WithObjects(class, gateway).
		Build()
	statusErr := errors.New("forced status patch failure")
	kubeClient := &patchFailingStatusClient{Client: baseClient, patchErr: statusErr}
	provider := &recordingProvider{}
	reconciler := Reconciler{
		Client: kubeClient, APIReader: kubeClient, Provider: provider,
		Coordinator: &graph.Coordinator{}, Config: cfg,
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gateway)}

	result, err := reconciler.Reconcile(context.Background(), request)
	if !errors.Is(err, statusErr) {
		t.Fatalf("first Reconcile() error = %v, want status patch failure", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("first Reconcile() result = %#v, want zero result", result)
	}
	if len(provider.deletedGateways) != 0 {
		t.Fatalf("DeleteGateway calls before status checkpoint = %d, want 0", len(provider.deletedGateways))
	}
	current := &gatewayv1.Gateway{}
	if err := baseClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), current); err != nil {
		t.Fatal(err)
	}
	if !controller.GatewayHasControllerBinding(cfg, current) {
		t.Fatal("status patch failure removed the Gateway binding")
	}
	programmed := meta.FindStatusCondition(current.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	if programmed == nil || programmed.Status != metav1.ConditionTrue {
		t.Fatalf("Programmed condition after failed checkpoint = %#v", programmed)
	}

	kubeClient.patchErr = nil
	if result, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	} else if result != (ctrl.Result{Requeue: true}) {
		t.Fatalf("second Reconcile() result = %#v, want status checkpoint", result)
	}
	if len(provider.deletedGateways) != 0 {
		t.Fatalf("DeleteGateway calls while publishing status = %d, want 0", len(provider.deletedGateways))
	}
	if err := baseClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), current); err != nil {
		t.Fatal(err)
	}
	programmed = meta.FindStatusCondition(current.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	if programmed == nil || programmed.Status != metav1.ConditionFalse {
		t.Fatalf("Programmed condition after checkpoint = %#v", programmed)
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("third Reconcile() error = %v", err)
	}
	if len(provider.deletedGateways) != 1 {
		t.Fatalf("DeleteGateway calls after status checkpoint = %d, want 1", len(provider.deletedGateways))
	}
}

func TestGatewayProviderFailureObservedGeneration(t *testing.T) {
	tests := []struct {
		name           string
		category       cloud.ErrorCategory
		wantProgrammed int64
	}{
		{name: "transient failure preserves the last cloud evaluation", category: cloud.ErrorCategoryRateLimit, wantProgrammed: 1},
		{name: "definitive failure records the evaluated generation", category: cloud.ErrorCategoryQuota, wantProgrammed: 2},
	}

	for _, test := range tests {
		t.Run("Gateway "+test.name, func(t *testing.T) {
			cfg := testConfig()
			gateway := programmedTestGateway(cfg)
			gateway.Generation = 2
			gateway.Status.Listeners = []gatewayv1.ListenerStatus{{
				Name: "http",
				Conditions: []metav1.Condition{
					Condition(string(gatewayv1.ListenerConditionAccepted), metav1.ConditionTrue, string(gatewayv1.ListenerReasonAccepted), "Listener is accepted", 1),
					Condition(string(gatewayv1.ListenerConditionResolvedRefs), metav1.ConditionTrue, string(gatewayv1.ListenerReasonResolvedRefs), "References are resolved", 1),
					Condition(string(gatewayv1.ListenerConditionProgrammed), metav1.ConditionTrue, string(gatewayv1.ListenerReasonProgrammed), "Listener is programmed", 1),
				},
			}}
			kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
				WithStatusSubresource(gateway).
				WithObjects(gateway).
				Build()
			reconciler := Reconciler{Client: kubeClient, Config: cfg}
			providerErr := cloud.NewProviderError(test.category, errors.New("provider failure"))

			scope := &gatewayScope{gateway: gateway, original: gateway.DeepCopy()}
			_, _ = reconciler.handleGatewayProviderFailure(context.Background(), scope, &gateway.Spec.Listeners[0], providerErr)
			if err := reconciler.patchGateway(context.Background(), scope); err != nil {
				t.Fatalf("patchGateway() error = %v", err)
			}
			current := &gatewayv1.Gateway{}
			if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), current); err != nil {
				t.Fatal(err)
			}
			accepted := meta.FindStatusCondition(current.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
			programmed := meta.FindStatusCondition(current.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
			listenerAccepted := meta.FindStatusCondition(current.Status.Listeners[0].Conditions, string(gatewayv1.ListenerConditionAccepted))
			listenerProgrammed := meta.FindStatusCondition(current.Status.Listeners[0].Conditions, string(gatewayv1.ListenerConditionProgrammed))
			if accepted == nil || accepted.ObservedGeneration != 2 || listenerAccepted == nil || listenerAccepted.ObservedGeneration != 2 {
				t.Fatalf("Accepted conditions = Gateway %#v, Listener %#v", accepted, listenerAccepted)
			}
			if programmed == nil || programmed.ObservedGeneration != test.wantProgrammed || listenerProgrammed == nil || listenerProgrammed.ObservedGeneration != test.wantProgrammed {
				t.Fatalf("Programmed conditions = Gateway %#v, Listener %#v", programmed, listenerProgrammed)
			}
		})

	}
}

func assertJitteredRequeue(t *testing.T, got, base time.Duration) {
	t.Helper()
	if base == 0 {
		if got != 0 {
			t.Fatalf("RequeueAfter = %s, want zero", got)
		}
		return
	}
	if got < base*8/10 || got > base*12/10 {
		t.Fatalf("RequeueAfter = %s, want a value from %s through %s", got, base*8/10, base*12/10)
	}
}

func receiveEvent(t *testing.T, recorder *events.FakeRecorder) string {
	t.Helper()
	select {
	case event := <-recorder.Events:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Event")
		return ""
	}
}

func assertNoEvent(t *testing.T, recorder *events.FakeRecorder) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		t.Fatalf("unexpected Event %q", event)
	case <-time.After(25 * time.Millisecond):
	}
}

func TestGatewayProgressingOutcomeUsesBoundedRequeue(t *testing.T) {
	cfg := testConfig()
	scheme := testScheme(t)
	class := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	gateway := programmedTestGateway(cfg)
	kubeClient := indexedFakeClientBuilder(scheme, cfg).
		WithStatusSubresource(gateway).
		WithObjects(class, gateway).
		Build()
	providerResult := cloud.GatewayProgressingResult("Octavia load balancer is PENDING_UPDATE", time.Hour)
	provider := &recordingProvider{gatewayResult: &providerResult}
	reconciler := Reconciler{
		Client:      kubeClient,
		APIReader:   kubeClient,
		Provider:    provider,
		Coordinator: &graph.Coordinator{},
		Config:      cfg,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	wantRequeue := controller.ProviderProgressRequeueAfter(providerResult.Outcome, gateway.UID)
	if result.RequeueAfter != wantRequeue {
		t.Fatalf("Reconcile() RequeueAfter = %s, want %s", result.RequeueAfter, wantRequeue)
	}
	if len(provider.gatewaySpecs) != 1 {
		t.Fatalf("EnsureGateway calls = %d, want 1", len(provider.gatewaySpecs))
	}
	current := &gatewayv1.Gateway{}
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}, current); err != nil {
		t.Fatal(err)
	}
	programmed := meta.FindStatusCondition(current.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	if programmed == nil || programmed.Status != metav1.ConditionFalse ||
		programmed.Reason != string(gatewayv1.GatewayReasonNoResources) ||
		programmed.Message != "Octavia load balancer is PENDING_UPDATE" {
		t.Fatalf("Programmed condition = %#v", programmed)
	}
	firstResourceVersion := current.ResourceVersion
	firstTransitionTime := programmed.LastTransitionTime

	secondResult, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}})
	if err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if secondResult.RequeueAfter != wantRequeue {
		t.Fatalf("second Reconcile() RequeueAfter = %s, want %s", secondResult.RequeueAfter, wantRequeue)
	}
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}, current); err != nil {
		t.Fatal(err)
	}
	programmed = meta.FindStatusCondition(current.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	if programmed == nil || !programmed.LastTransitionTime.Equal(&firstTransitionTime) {
		t.Fatalf("Programmed LastTransitionTime = %#v, want %s", programmed, firstTransitionTime)
	}
	if current.ResourceVersion != firstResourceVersion {
		t.Fatalf("Gateway resource version changed from %q to %q after a semantic no-op", firstResourceVersion, current.ResourceVersion)
	}
}

func TestGatewayProviderErrorTakesPrecedenceOverProgress(t *testing.T) {
	cfg := testConfig()
	scheme := testScheme(t)
	class := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	gateway := programmedTestGateway(cfg)
	kubeClient := indexedFakeClientBuilder(scheme, cfg).
		WithStatusSubresource(gateway).
		WithObjects(class, gateway).
		Build()
	providerResult := cloud.GatewayProgressingResult("this progress must be ignored", time.Second)
	providerErr := cloud.NewProviderError(cloud.ErrorCategoryRetryableService, errors.New("Octavia is unavailable"))
	provider := &recordingProvider{gatewayResult: &providerResult, gatewayErr: providerErr}
	reconciler := Reconciler{
		Client:      kubeClient,
		APIReader:   kubeClient,
		Provider:    provider,
		Coordinator: &graph.Coordinator{},
		Config:      cfg,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}})
	if !errors.Is(err, cloud.ErrRetryableService) {
		t.Fatalf("Reconcile() error = %v, want retryable service error", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("Reconcile() result = %#v, want no explicit requeue when an error is returned", result)
	}
	current := &gatewayv1.Gateway{}
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name}, current); err != nil {
		t.Fatal(err)
	}
	programmed := meta.FindStatusCondition(current.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	if programmed == nil || programmed.Message == "this progress must be ignored" {
		t.Fatalf("Programmed condition = %#v", programmed)
	}
}

func TestGatewayProviderFailureDuringFinalizationKeepsBinding(t *testing.T) {
	cfg := testConfig()
	scheme := testScheme(t)
	now := metav1.Now()
	gateway := programmedTestGateway(cfg)
	gateway.DeletionTimestamp = &now
	gateway.Status.Listeners = []gatewayv1.ListenerStatus{{
		Name: "http",
		Conditions: []metav1.Condition{
			Condition(string(gatewayv1.ListenerConditionAccepted), metav1.ConditionTrue, string(gatewayv1.ListenerReasonAccepted), "Listener is accepted", gateway.Generation),
			Condition(string(gatewayv1.ListenerConditionProgrammed), metav1.ConditionTrue, string(gatewayv1.ListenerReasonProgrammed), "Listener is programmed", gateway.Generation),
		},
	}}
	providerErr := cloud.NewProviderError(cloud.ErrorCategoryTerminalValidation, errors.New("token=secret-token"))
	provider := &recordingProvider{gatewayDeleteErr: providerErr}
	recorder := events.NewFakeRecorder(1)
	kubeClient := indexedFakeClientBuilder(scheme, cfg).
		WithStatusSubresource(gateway).
		WithObjects(gateway).
		Build()
	reconciler := Reconciler{
		Client: kubeClient, APIReader: kubeClient, Provider: provider,
		Coordinator: &graph.Coordinator{}, Recorder: recorder, Config: cfg,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gateway)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	assertJitteredRequeue(t, result.RequeueAfter, providerFailureRequeue)
	current := &gatewayv1.Gateway{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), current); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(current.Finalizers, cfg.GatewayFinalizer()) || current.Annotations[cfg.GatewayProjectIDAnnotation()] == "" {
		t.Fatalf("Gateway binding was removed: finalizers %#v, annotations %#v", current.Finalizers, current.Annotations)
	}
	programmed := meta.FindStatusCondition(current.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	accepted := meta.FindStatusCondition(current.Status.Listeners[0].Conditions, string(gatewayv1.ListenerConditionAccepted))
	if programmed == nil || programmed.Reason != string(gatewayv1.GatewayReasonInvalid) || strings.Contains(programmed.Message, "secret-token") {
		t.Fatalf("Gateway Programmed condition = %#v", programmed)
	}
	if accepted == nil || accepted.Status != metav1.ConditionTrue {
		t.Fatalf("Listener Accepted condition = %#v", accepted)
	}
	if event := receiveEvent(t, recorder); !strings.Contains(event, "InvalidProviderConfiguration") || strings.Contains(event, "secret-token") {
		t.Fatalf("Event = %q", event)
	}
}

type patchFailingStatusClient struct {
	client.Client
	patchErr error
}

func (c *patchFailingStatusClient) Status() client.StatusWriter {
	return &patchFailingStatusWriter{SubResourceWriter: c.Client.Status(), patchErr: c.patchErr}
}

type patchFailingStatusWriter struct {
	client.SubResourceWriter
	patchErr error
}

func (w *patchFailingStatusWriter) Patch(
	ctx context.Context,
	object client.Object,
	patch client.Patch,
	options ...client.SubResourcePatchOption,
) error {
	if w.patchErr != nil {
		return w.patchErr
	}
	return w.SubResourceWriter.Patch(ctx, object, patch, options...)
}
