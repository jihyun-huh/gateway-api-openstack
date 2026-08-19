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

package httproute

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
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

func TestHTTPRouteProviderFailurePreservesProviderAndStatusErrors(t *testing.T) {
	cfg := testConfig()
	parent := gatewayv1.ParentReference{Name: "edge"}
	class, gateway := providerRouteParentObjects(cfg)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "api", UID: "route-uid", Generation: 1},
		Spec:       gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{parent}}},
	}
	baseClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(route).
		WithObjects(class, gateway, route).
		Build()
	statusErr := errors.New("forced status patch failure")
	kubeClient := &patchFailingStatusClient{Client: baseClient, patchErr: statusErr}
	reconciler := Reconciler{Client: kubeClient, Config: cfg}
	providerErr := cloud.NewProviderError(cloud.ErrorCategoryRetryableService, errors.New("token=secret-token"))

	scope := &httpRouteScope{route: route}
	_, reconcileErr := reconciler.handleRouteProviderFailure(scope, parent, true, providerErr, "EnsureHTTPRoute")
	patchErr := reconciler.patchHTTPRoute(context.Background(), scope)
	err := errors.Join(reconcileErr, scope.patchCause, patchErr)
	if !errors.Is(err, providerErr) || !errors.Is(err, statusErr) {
		t.Fatalf("handleRouteProviderFailure() error = %v", err)
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("handleRouteProviderFailure() exposed provider cause: %q", err)
	}
}

func TestHTTPRouteProviderFailureObservedGeneration(t *testing.T) {
	tests := []struct {
		name           string
		category       cloud.ErrorCategory
		wantProgrammed int64
	}{
		{name: "transient failure preserves the last cloud evaluation", category: cloud.ErrorCategoryRateLimit, wantProgrammed: 1},
		{name: "definitive failure records the evaluated generation", category: cloud.ErrorCategoryQuota, wantProgrammed: 2},
	}

	for _, test := range tests {
		t.Run("HTTPRoute "+test.name, func(t *testing.T) {
			cfg := testConfig()
			parent := gatewayv1.ParentReference{Name: "edge"}
			class, gateway := providerRouteParentObjects(cfg)
			route := &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "api", UID: "route-uid", Generation: 2},
				Spec:       gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{parent}}},
				Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{{
					ParentRef: parent, ControllerName: cfg.ControllerName,
					Conditions: []metav1.Condition{
						Condition(string(gatewayv1.RouteConditionAccepted), metav1.ConditionTrue, string(gatewayv1.RouteReasonAccepted), "HTTPRoute is accepted", 1),
						Condition(string(gatewayv1.RouteConditionResolvedRefs), metav1.ConditionTrue, string(gatewayv1.RouteReasonResolvedRefs), "References are resolved", 1),
						Condition(cfg.Domain()+"/Programmed", metav1.ConditionTrue, "Programmed", "HTTPRoute is programmed", 1),
					},
				}}}},
			}
			kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
				WithStatusSubresource(route).
				WithObjects(class, gateway, route).
				Build()
			reconciler := Reconciler{Client: kubeClient, Config: cfg}
			providerErr := cloud.NewProviderError(test.category, errors.New("provider failure"))

			scope := &httpRouteScope{route: route}
			_, _ = reconciler.handleRouteProviderFailure(scope, parent, true, providerErr, "EnsureHTTPRoute")
			if err := reconciler.patchHTTPRoute(context.Background(), scope); err != nil {
				t.Fatalf("patchHTTPRoute() error = %v", err)
			}
			current := &gatewayv1.HTTPRoute{}
			if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
				t.Fatal(err)
			}
			accepted := meta.FindStatusCondition(current.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
			resolved := meta.FindStatusCondition(current.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
			programmed := meta.FindStatusCondition(current.Status.Parents[0].Conditions, cfg.Domain()+"/Programmed")
			if accepted == nil || accepted.ObservedGeneration != 2 || resolved == nil || resolved.ObservedGeneration != 2 {
				t.Fatalf("Accepted = %#v, ResolvedRefs = %#v", accepted, resolved)
			}
			if programmed == nil || programmed.ObservedGeneration != test.wantProgrammed {
				t.Fatalf("Programmed condition = %#v", programmed)
			}
		})
	}
}

func TestHTTPRouteFailureBeforeBackendEvaluationPreservesResolvedGeneration(t *testing.T) {
	cfg := testConfig()
	parent := gatewayv1.ParentReference{Name: "edge"}
	class, gateway := providerRouteParentObjects(cfg)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "api", UID: "route-uid", Generation: 2},
		Spec:       gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{parent}}},
		Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{{
			ParentRef: parent, ControllerName: cfg.ControllerName,
			Conditions: []metav1.Condition{
				Condition(string(gatewayv1.RouteConditionAccepted), metav1.ConditionTrue, string(gatewayv1.RouteReasonAccepted), "HTTPRoute is accepted", 1),
				Condition(string(gatewayv1.RouteConditionResolvedRefs), metav1.ConditionTrue, string(gatewayv1.RouteReasonResolvedRefs), "References are resolved", 1),
				Condition(cfg.Domain()+"/Programmed", metav1.ConditionTrue, "Programmed", "HTTPRoute is programmed", 1),
			},
		}}}},
	}
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(route).
		WithObjects(class, gateway, route).
		Build()
	reconciler := Reconciler{Client: kubeClient, Config: cfg}
	providerErr := cloud.NewProviderError(cloud.ErrorCategoryRateLimit, errors.New("rate limited"))

	scope := &httpRouteScope{route: route}
	_, _ = reconciler.handleRouteProviderFailure(scope, parent, false, providerErr, "DeleteHTTPRoute")
	if err := reconciler.patchHTTPRoute(context.Background(), scope); err != nil {
		t.Fatalf("patchHTTPRoute() error = %v", err)
	}
	current := &gatewayv1.HTTPRoute{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	accepted := meta.FindStatusCondition(current.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
	resolved := meta.FindStatusCondition(current.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
	programmed := meta.FindStatusCondition(current.Status.Parents[0].Conditions, cfg.Domain()+"/Programmed")
	if accepted == nil || accepted.ObservedGeneration != 2 {
		t.Fatalf("Accepted condition = %#v", accepted)
	}
	if resolved == nil || resolved.Status != metav1.ConditionUnknown || resolved.ObservedGeneration != 1 {
		t.Fatalf("ResolvedRefs condition = %#v", resolved)
	}
	if programmed == nil || programmed.ObservedGeneration != 1 {
		t.Fatalf("Programmed condition = %#v", programmed)
	}
}

func providerRouteParentObjects(cfg Config) (*gatewayv1.GatewayClass, *gatewayv1.Gateway) {
	class := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	return class, programmedTestGateway(cfg)
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

func TestHTTPRouteProgressingOutcomeUsesBoundedRequeue(t *testing.T) {
	providerResult := cloud.RouteProgressingResult("Octavia pool is PENDING_CREATE", time.Millisecond)
	provider := &recordingProvider{routeResult: &providerResult}
	reconciler, kubeClient, route := providerOutcomeRouteFixture(t, provider, nil)
	cfg := reconciler.Config

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: route.Namespace, Name: route.Name}})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	wantRequeue := controller.ProviderProgressRequeueAfter(providerResult.Outcome, route.UID)
	if result.RequeueAfter != wantRequeue {
		t.Fatalf("Reconcile() RequeueAfter = %s, want %s", result.RequeueAfter, wantRequeue)
	}
	if len(provider.routeSpecs) != 1 {
		t.Fatalf("EnsureRoute calls = %d, want 1", len(provider.routeSpecs))
	}
	current := &gatewayv1.HTTPRoute{}
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: route.Namespace, Name: route.Name}, current); err != nil {
		t.Fatal(err)
	}
	if len(current.Status.Parents) != 1 {
		t.Fatalf("HTTPRoute parent statuses = %#v", current.Status.Parents)
	}
	programmed := meta.FindStatusCondition(current.Status.Parents[0].Conditions, cfg.Domain()+"/Programmed")
	if programmed == nil || programmed.Status != metav1.ConditionFalse || programmed.Reason != "Pending" ||
		programmed.Message != "Octavia pool is PENDING_CREATE" {
		t.Fatalf("Programmed condition = %#v", programmed)
	}
}

func TestBoundHTTPRouteReconcilesWhileGatewayIsProgressing(t *testing.T) {
	providerResult := cloud.RouteProgressingResult("Validating the HTTPRoute graph before restoring traffic", time.Second)
	provider := &recordingProvider{routeResult: &providerResult}
	reconciler, kubeClient, route := providerOutcomeRouteFixture(t, provider, nil)

	var gateway gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: route.Namespace, Name: "edge"}, &gateway); err != nil {
		t.Fatal(err)
	}
	SetCondition(&gateway.Status.Conditions, Condition(
		string(gatewayv1.GatewayConditionProgrammed),
		metav1.ConditionFalse,
		string(gatewayv1.GatewayReasonPending),
		"Waiting for HTTPRoute validation before restoring traffic",
		gateway.Generation,
	))
	if err := kubeClient.Status().Update(context.Background(), &gateway); err != nil {
		t.Fatalf("mark Gateway progressing: %v", err)
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(route)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(provider.routeSpecs) != 1 {
		t.Fatalf("EnsureRoute calls = %d, want 1", len(provider.routeSpecs))
	}
	got := provider.routeSpecs[0].Identity
	if got.GatewayUID != string(gateway.UID) || got.RouteUID != string(route.UID) {
		t.Fatalf("EnsureRoute identity = %#v, want Gateway %q and HTTPRoute %q", got, gateway.UID, route.UID)
	}
	wantRequeue := controller.ProviderProgressRequeueAfter(providerResult.Outcome, route.UID)
	if result.RequeueAfter != wantRequeue {
		t.Fatalf("Reconcile() RequeueAfter = %s, want %s", result.RequeueAfter, wantRequeue)
	}
}

func TestBoundHTTPRouteDoesNotReconcileInvalidGateway(t *testing.T) {
	provider := &recordingProvider{}
	reconciler, kubeClient, route := providerOutcomeRouteFixture(t, provider, nil)

	var gateway gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: route.Namespace, Name: "edge"}, &gateway); err != nil {
		t.Fatal(err)
	}
	gateway.Spec.Addresses = []gatewayv1.GatewaySpecAddress{{Value: "192.0.2.20"}}
	gateway.Generation++
	if err := kubeClient.Update(context.Background(), &gateway); err != nil {
		t.Fatalf("make Gateway unsupported: %v", err)
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(route)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatalf("Reconcile() result = %#v, want a delayed retry", result)
	}
	if len(provider.routeSpecs) != 0 {
		t.Fatalf("EnsureRoute calls for an invalid Gateway = %d, want 0", len(provider.routeSpecs))
	}
}

func TestHTTPRouteMutationBoundaryUsesAPIReader(t *testing.T) {
	provider := &recordingProvider{}
	reconciler, kubeClient, route := providerOutcomeRouteFixture(t, provider, nil)
	reconciler.APIReader = &deletingHTTPRouteReader{
		Reader: kubeClient,
		key:    client.ObjectKeyFromObject(route),
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(route)})
	if !errors.Is(err, errHTTPRouteChanged) {
		t.Fatalf("Reconcile() error = %v, want %v", err, errHTTPRouteChanged)
	}
	if len(provider.routeSpecs) != 0 {
		t.Fatalf("EnsureRoute calls after API server deletion = %d, want 0", len(provider.routeSpecs))
	}
}

func TestHTTPRouteProviderFailuresUseSafeStatusAndRetryPolicy(t *testing.T) {
	tests := []struct {
		name            string
		category        cloud.ErrorCategory
		wantStatus      metav1.ConditionStatus
		wantReason      string
		wantError       bool
		wantRequeueBase time.Duration
		wantEvent       bool
	}{
		{name: "rate limit", category: cloud.ErrorCategoryRateLimit, wantStatus: metav1.ConditionUnknown, wantReason: "RateLimited", wantError: true},
		{name: "ownership conflict", category: cloud.ErrorCategoryOwnershipConflict, wantStatus: metav1.ConditionFalse, wantReason: "OwnershipConflict", wantRequeueBase: ownershipConflictRequeue, wantEvent: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			providerErr := cloud.NewProviderError(test.category, errors.New("token=secret-token response=private-body"))
			provider := &recordingProvider{routeErr: providerErr}
			recorder := events.NewFakeRecorder(1)
			reconciler, kubeClient, route := providerOutcomeRouteFixture(t, provider, recorder)

			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(route)})
			if test.wantError {
				if !errors.Is(err, providerErr) || strings.Contains(err.Error(), "secret-token") {
					t.Fatalf("Reconcile() error = %v", err)
				}
			} else if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			assertJitteredRequeue(t, result.RequeueAfter, test.wantRequeueBase)

			current := &gatewayv1.HTTPRoute{}
			if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
				t.Fatal(err)
			}
			if len(current.Status.Parents) != 1 {
				t.Fatalf("HTTPRoute parent statuses = %#v", current.Status.Parents)
			}
			accepted := meta.FindStatusCondition(current.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
			resolved := meta.FindStatusCondition(current.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
			programmed := meta.FindStatusCondition(current.Status.Parents[0].Conditions, reconciler.Config.Domain()+"/Programmed")
			if accepted == nil || accepted.Status != metav1.ConditionTrue || resolved == nil || resolved.Status != metav1.ConditionTrue {
				t.Fatalf("Accepted = %#v, ResolvedRefs = %#v", accepted, resolved)
			}
			if programmed == nil || programmed.Status != test.wantStatus || programmed.Reason != test.wantReason || strings.Contains(programmed.Message, "secret-token") {
				t.Fatalf("Programmed condition = %#v", programmed)
			}
			if test.wantEvent {
				if event := receiveEvent(t, recorder); !strings.Contains(event, test.wantReason) || strings.Contains(event, "secret-token") {
					t.Fatalf("Event = %q", event)
				}
				if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(route)}); err != nil {
					t.Fatalf("second Reconcile() error = %v", err)
				}
				assertNoEvent(t, recorder)
			} else {
				assertNoEvent(t, recorder)
			}
		})
	}

	policy, _ := controller.ProviderFailurePolicyFor(cloud.NewProviderError(cloud.ErrorCategoryRetryableService, errors.New("unavailable")))
	status := providerFailureRouteStatus(policy, false)
	if status.resolved != metav1.ConditionUnknown || status.resolvedReason != string(gatewayv1.RouteReasonPending) {
		t.Fatalf("status before backend evaluation = %#v", status)
	}
}

func providerOutcomeRouteFixture(
	t *testing.T,
	provider *recordingProvider,
	recorder events.EventRecorder,
) (*Reconciler, client.Client, *gatewayv1.HTTPRoute) {
	t.Helper()
	cfg := testConfig()
	scheme := testScheme(t)
	class := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName},
	}
	gateway := programmedTestGateway(cfg)
	pathType := gatewayv1.PathMatchPathPrefix
	pathValue := "/api"
	backendPort := gatewayv1.PortNumber(8080)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "api", UID: "route-uid", Generation: 1},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "edge"}}},
			Rules: []gatewayv1.HTTPRouteRule{{
				Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: &pathType, Value: &pathValue}}},
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
					Name: "backend",
					Port: &backendPort,
				}}}},
			}},
		},
	}
	(&Reconciler{Config: cfg}).applyRouteBinding(route, gateway)
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "backend"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Ports: []corev1.ServicePort{{Port: 8080, NodePort: 30080}}},
	}
	ready := true
	endpointSlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "backend-1", Labels: map[string]string{discoveryv1.LabelServiceName: "backend"}},
		Endpoints:  []discoveryv1.Endpoint{{Conditions: discoveryv1.EndpointConditions{Ready: &ready}}},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Status: corev1.NodeStatus{
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.11"}},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	kubeClient := indexedFakeClientBuilder(scheme, cfg).
		WithStatusSubresource(gateway, route).
		WithObjects(class, gateway, route, service, endpointSlice, node).
		Build()
	reconciler := &Reconciler{
		Client: kubeClient, APIReader: kubeClient, Provider: provider,
		Coordinator: &graph.Coordinator{}, Recorder: recorder, Config: cfg,
	}
	return reconciler, kubeClient, route
}

type deletingHTTPRouteReader struct {
	client.Reader
	key client.ObjectKey
}

func (r *deletingHTTPRouteReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	if err := r.Reader.Get(ctx, key, object, options...); err != nil {
		return err
	}
	if key != r.key {
		return nil
	}
	route, ok := object.(*gatewayv1.HTTPRoute)
	if !ok {
		return nil
	}
	now := metav1.Now()
	route.DeletionTimestamp = &now
	return nil
}

func TestHTTPRouteProviderFailureDuringFinalizationPreservesStatusAndBinding(t *testing.T) {
	cfg := testConfig()
	scheme := testScheme(t)
	now := metav1.Now()
	parent := gatewayv1.ParentReference{Name: "edge"}
	foreignStatus := gatewayv1.RouteParentStatus{
		ParentRef:      parent,
		ControllerName: "other.example/controller",
		Conditions: []metav1.Condition{
			Condition(string(gatewayv1.RouteConditionAccepted), metav1.ConditionFalse, string(gatewayv1.RouteReasonNotAllowedByListeners), "Preserve this status", 1),
		},
	}
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "api", UID: "route-uid", Generation: 1,
			DeletionTimestamp: &now,
			Finalizers:        routeBindingFinalizers(cfg, "default", "edge", "gateway-uid"),
			Annotations: map[string]string{
				cfg.RouteGatewayNamespaceAnnotation(): "default",
				cfg.RouteGatewayNameAnnotation():      "edge",
				cfg.RouteGatewayUIDAnnotation():       "gateway-uid",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{parent}}},
		Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{
			{
				ParentRef: parent, ControllerName: cfg.ControllerName,
				Conditions: []metav1.Condition{
					Condition(string(gatewayv1.RouteConditionAccepted), metav1.ConditionTrue, string(gatewayv1.RouteReasonAccepted), "HTTPRoute is accepted", 1),
					Condition(string(gatewayv1.RouteConditionResolvedRefs), metav1.ConditionTrue, string(gatewayv1.RouteReasonResolvedRefs), "Backend references are resolved", 1),
					Condition(cfg.Domain()+"/Programmed", metav1.ConditionTrue, "Programmed", "HTTPRoute is programmed", 1),
				},
			},
			foreignStatus,
		}}},
	}
	providerErr := cloud.NewProviderError(cloud.ErrorCategoryOwnershipConflict, errors.New("project=private-project response=private-body"))
	provider := &recordingProvider{routeDeleteErr: providerErr}
	recorder := events.NewFakeRecorder(1)
	kubeClient := indexedFakeClientBuilder(scheme, cfg).
		WithStatusSubresource(route).
		WithObjects(route).
		Build()
	reconciler := Reconciler{
		Client: kubeClient, APIReader: kubeClient, Provider: provider,
		Coordinator: &graph.Coordinator{}, Recorder: recorder, Config: cfg,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(route)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	assertJitteredRequeue(t, result.RequeueAfter, ownershipConflictRequeue)
	current := &gatewayv1.HTTPRoute{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	for _, finalizer := range route.Finalizers {
		if !slices.Contains(current.Finalizers, finalizer) {
			t.Errorf("HTTPRoute finalizer %q was removed", finalizer)
		}
	}
	if current.Annotations[cfg.RouteGatewayUIDAnnotation()] != "gateway-uid" {
		t.Fatalf("HTTPRoute binding annotations = %#v", current.Annotations)
	}
	if current.Annotations[cfg.RouteCleanupFailureAnnotation()] != controller.ProviderReasonOwnershipConflict {
		t.Fatalf("HTTPRoute cleanup failure checkpoint = %q", current.Annotations[cfg.RouteCleanupFailureAnnotation()])
	}
	if len(current.Status.Parents) != 2 || !controller.ParentRefsEqual(current.Status.Parents[0].ParentRef, current.Namespace, parent, current.Namespace) {
		t.Fatalf("HTTPRoute parent statuses = %#v", current.Status.Parents)
	}
	accepted := meta.FindStatusCondition(current.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
	resolved := meta.FindStatusCondition(current.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
	programmed := meta.FindStatusCondition(current.Status.Parents[0].Conditions, cfg.Domain()+"/Programmed")
	if accepted == nil || accepted.Status != metav1.ConditionTrue || resolved == nil || resolved.Status != metav1.ConditionTrue {
		t.Fatalf("Accepted = %#v, ResolvedRefs = %#v", accepted, resolved)
	}
	if programmed == nil || programmed.Status != metav1.ConditionFalse || programmed.Reason != "OwnershipConflict" || strings.Contains(programmed.Message, "private-project") {
		t.Fatalf("Programmed condition = %#v", programmed)
	}
	if got := current.Status.Parents[1]; !equalRouteParentStatus(got, foreignStatus) {
		t.Fatalf("foreign parent status = %#v, want %#v", got, foreignStatus)
	}
	if event := receiveEvent(t, recorder); !strings.Contains(event, "OwnershipConflict") || strings.Contains(event, "private-project") {
		t.Fatalf("Event = %q", event)
	}
}

func TestHTTPRouteDetachFailurePublishesCleanupStatus(t *testing.T) {
	cfg := testConfig()
	parent := gatewayv1.ParentReference{Name: "edge"}
	class, gateway := providerRouteParentObjects(cfg)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "api", UID: "route-uid", Generation: 1,
			Finalizers: routeBindingFinalizers(cfg, "default", "edge", "gateway-uid"),
			Annotations: map[string]string{
				cfg.RouteGatewayNamespaceAnnotation(): "default",
				cfg.RouteGatewayNameAnnotation():      "edge",
				cfg.RouteGatewayUIDAnnotation():       "gateway-uid",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{parent}}},
	}
	providerErr := cloud.NewProviderError(cloud.ErrorCategoryOwnershipConflict, errors.New("response=private-body"))
	provider := &recordingProvider{routeDeleteErr: providerErr}
	recorder := events.NewFakeRecorder(1)
	updates := []parentStatusUpdate{{
		parent: parent,
		status: rejectedRouteStatus(string(gatewayv1.RouteReasonUnsupportedValue), "HTTPRoute configuration is unsupported"),
	}}
	applyRouteParentStatuses(cfg, route, updates)
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(route).
		WithObjects(class, gateway, route).
		Build()
	current := &gatewayv1.HTTPRoute{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	reconciler := Reconciler{
		Client: kubeClient, APIReader: kubeClient, Provider: provider,
		Coordinator: &graph.Coordinator{}, Recorder: recorder, Config: cfg,
	}
	scope := &httpRouteScope{route: current}
	result, err := reconciler.setRouteStatusesAndDetach(context.Background(), scope, updates)
	if err != nil {
		t.Fatalf("setRouteStatusesAndDetach() error = %v", err)
	}
	if err := reconciler.patchHTTPRoute(context.Background(), scope); err != nil {
		t.Fatalf("patchHTTPRoute() error = %v", err)
	}
	assertJitteredRequeue(t, result.RequeueAfter, ownershipConflictRequeue)
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(current.Finalizers, cfg.RouteFinalizer()) || current.Annotations[cfg.RouteGatewayUIDAnnotation()] != "gateway-uid" {
		t.Fatalf("HTTPRoute binding was removed: finalizers %#v, annotations %#v", current.Finalizers, current.Annotations)
	}
	if current.Annotations[cfg.RouteCleanupFailureAnnotation()] != controller.ProviderReasonOwnershipConflict {
		t.Fatalf("HTTPRoute cleanup failure checkpoint = %q", current.Annotations[cfg.RouteCleanupFailureAnnotation()])
	}
	programmed := meta.FindStatusCondition(current.Status.Parents[0].Conditions, cfg.Domain()+"/Programmed")
	if programmed == nil || programmed.Reason != "OwnershipConflict" || !strings.Contains(programmed.Message, "HTTPRoute cleanup") || strings.Contains(programmed.Message, "private-body") {
		t.Fatalf("Programmed condition = %#v", programmed)
	}
	if event := receiveEvent(t, recorder); !strings.Contains(event, "HTTPRoute cleanup") || !strings.Contains(event, "OwnershipConflict") {
		t.Fatalf("Event = %q", event)
	}

	scope = &httpRouteScope{route: current}
	if _, err := reconciler.setRouteStatusesAndDetach(context.Background(), scope, updates); err != nil {
		t.Fatalf("second setRouteStatusesAndDetach() error = %v", err)
	}
	if err := reconciler.patchHTTPRoute(context.Background(), scope); err != nil {
		t.Fatalf("second patchHTTPRoute() error = %v", err)
	}
	assertNoEvent(t, recorder)

	provider.routeDeleteErr = nil
	progressing := cloud.ProgressingOutcome("HTTPRoute cleanup is progressing", time.Second)
	provider.routeDeleteOut = &progressing
	scope = &httpRouteScope{route: current}
	result, err = reconciler.setRouteStatusesAndDetach(context.Background(), scope, updates)
	if err != nil {
		t.Fatalf("progressing setRouteStatusesAndDetach() error = %v", err)
	}
	if err := reconciler.patchHTTPRoute(context.Background(), scope); err != nil {
		t.Fatalf("progressing patchHTTPRoute() error = %v", err)
	}
	wantRequeue := controller.ProviderProgressRequeueAfter(progressing, current.UID)
	if result.RequeueAfter != wantRequeue {
		t.Fatalf("progressing RequeueAfter = %s, want %s", result.RequeueAfter, wantRequeue)
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	if _, present := current.Annotations[cfg.RouteCleanupFailureAnnotation()]; present {
		t.Fatalf("progressing cleanup retained failure checkpoint: %#v", current.Annotations)
	}
}

func TestHTTPRouteFinalizationWithoutStoredParentStatusEmitsEvent(t *testing.T) {
	for _, test := range []struct {
		name        string
		specGateway string
	}{
		{name: "stored parent remains in spec", specGateway: "old-gateway"},
		{name: "stored parent was removed from spec", specGateway: "new-gateway"},
	} {
		t.Run(test.name, func(t *testing.T) {
			testHTTPRouteFinalizationWithoutParentStatus(t, test.specGateway)
		})
	}
}

func testHTTPRouteFinalizationWithoutParentStatus(t *testing.T, specGateway string) {
	t.Helper()
	cfg := testConfig()
	scheme := testScheme(t)
	now := metav1.Now()
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "api", UID: "route-uid", Generation: 1,
			DeletionTimestamp: &now,
			Finalizers:        routeBindingFinalizers(cfg, "default", "old-gateway", "old-gateway-uid"),
			Annotations: map[string]string{
				cfg.RouteGatewayNamespaceAnnotation(): "default",
				cfg.RouteGatewayNameAnnotation():      "old-gateway",
				cfg.RouteGatewayUIDAnnotation():       "old-gateway-uid",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: gatewayv1.ObjectName(specGateway)}}}},
	}
	providerErr := cloud.NewProviderError(cloud.ErrorCategoryTerminalValidation, errors.New("invalid delete request"))
	provider := &recordingProvider{routeDeleteErr: providerErr}
	recorder := events.NewFakeRecorder(1)
	kubeClient := indexedFakeClientBuilder(scheme, cfg).WithStatusSubresource(route).WithObjects(route).Build()
	reconciler := Reconciler{Client: kubeClient, Provider: provider, Coordinator: &graph.Coordinator{}, APIReader: kubeClient, Recorder: recorder, Config: cfg}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(route)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	assertJitteredRequeue(t, result.RequeueAfter, providerFailureRequeue)
	current := &gatewayv1.HTTPRoute{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	if len(current.Status.Parents) != 0 {
		t.Fatalf("HTTPRoute parent statuses = %#v, want none", current.Status.Parents)
	}
	if !slices.Contains(current.Finalizers, cfg.RouteFinalizer()) {
		t.Fatal("HTTPRoute cleanup failure removed the finalizer")
	}
	if current.Annotations[cfg.RouteCleanupFailureAnnotation()] != controller.ProviderReasonInvalidConfiguration {
		t.Fatalf("HTTPRoute cleanup failure checkpoint = %q", current.Annotations[cfg.RouteCleanupFailureAnnotation()])
	}
	if event := receiveEvent(t, recorder); !strings.Contains(event, "InvalidProviderConfiguration") || !strings.Contains(event, "HTTPRoute cleanup") {
		t.Fatalf("Event = %q", event)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(route)}); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	assertNoEvent(t, recorder)
}

func equalRouteParentStatus(left, right gatewayv1.RouteParentStatus) bool {
	return controller.ParentRefsEqual(left.ParentRef, "default", right.ParentRef, "default") &&
		left.ControllerName == right.ControllerName &&
		len(left.Conditions) == len(right.Conditions) &&
		left.Conditions[0] == right.Conditions[0]
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
