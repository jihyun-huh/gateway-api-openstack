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
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

func TestHTTPRouteDeferredStatusRetryPreservesConcurrentUpdates(t *testing.T) {
	cfg := testConfig()
	parent := gatewayv1.ParentReference{Name: "edge"}
	class, gateway := providerRouteParentObjects(cfg)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "api", UID: "route-uid", Generation: 1,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{parent}},
		},
	}
	baseClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(route).
		WithObjects(class, gateway, route).
		Build()
	foreignController := gatewayv1.GatewayController("example.net/other-controller")
	foreignParent := gatewayv1.RouteParentStatus{
		ParentRef:      gatewayv1.ParentReference{Name: "other-gateway"},
		ControllerName: foreignController,
		Conditions: []metav1.Condition{
			condition("example.net/Ready", metav1.ConditionTrue, "Ready", "Foreign controller is ready", 1),
		},
	}
	foreignCondition := condition(
		"example.net/Audited",
		metav1.ConditionTrue,
		"Audited",
		"Another writer audited this parent",
		1,
	)
	kubeClient := &conflictOnceRouteStatusClient{
		Client: baseClient,
		mutate: func(current *gatewayv1.HTTPRoute) {
			current.Status.Parents = []gatewayv1.RouteParentStatus{
				foreignParent,
				{
					ParentRef:      parent,
					ControllerName: cfg.ControllerName,
					Conditions:     []metav1.Condition{foreignCondition},
				},
			}
		},
	}
	reconciler := &HTTPRouteReconciler{
		Client: kubeClient, APIReader: baseClient, Config: cfg,
	}
	scope := &httpRouteScope{route: route.DeepCopy()}
	scope.setStatuses([]parentStatusUpdate{{parent: parent, status: programmedRouteStatus()}})

	if err := reconciler.patchHTTPRoute(context.Background(), scope); err != nil {
		t.Fatalf("patchHTTPRoute() error = %v", err)
	}
	if kubeClient.patchCalls != 2 {
		t.Fatalf("status patch calls = %d, want conflict and retry", kubeClient.patchCalls)
	}

	current := &gatewayv1.HTTPRoute{}
	if err := baseClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	if len(current.Status.Parents) != 2 {
		t.Fatalf("HTTPRoute parent statuses = %#v, want both controllers", current.Status.Parents)
	}
	var ownStatus *gatewayv1.RouteParentStatus
	for index := range current.Status.Parents {
		status := &current.Status.Parents[index]
		if status.ControllerName == foreignController {
			if !equalRouteParentStatus(*status, foreignParent) {
				t.Fatalf("foreign parent status = %#v, want %#v", *status, foreignParent)
			}
			continue
		}
		if status.ControllerName == cfg.ControllerName {
			ownStatus = status
		}
	}
	if ownStatus == nil {
		t.Fatal("controller parent status was not written")
	}
	if condition := findCondition(ownStatus.Conditions, foreignCondition.Type); condition == nil || *condition != foreignCondition {
		t.Fatalf("concurrent condition = %#v, want %#v", condition, foreignCondition)
	}
	if condition := findCondition(ownStatus.Conditions, cfg.domain()+"/Programmed"); condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("Programmed condition = %#v", condition)
	}
}

func TestHTTPRouteDetachWaitsForStatusCheckpoint(t *testing.T) {
	cfg := testConfig()
	parent := gatewayv1.ParentReference{Name: "old-gateway"}
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "api", UID: "route-uid", Generation: 1,
			Finalizers: routeBindingFinalizers(cfg, "default", "old-gateway", "old-gateway-uid"),
			Annotations: map[string]string{
				cfg.routeGatewayNamespaceAnnotation(): "default",
				cfg.routeGatewayNameAnnotation():      "old-gateway",
				cfg.routeGatewayUIDAnnotation():       "old-gateway-uid",
			},
		},
		Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{{
			ParentRef:      parent,
			ControllerName: cfg.ControllerName,
			Conditions: []metav1.Condition{
				condition(string(gatewayv1.RouteConditionAccepted), metav1.ConditionTrue, string(gatewayv1.RouteReasonAccepted), "HTTPRoute is accepted", 1),
			},
		}}}},
	}
	baseClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(route).
		WithObjects(route).
		Build()
	statusErr := errors.New("forced HTTPRoute status patch failure")
	kubeClient := &patchFailingStatusClient{Client: baseClient, patchErr: statusErr}
	provider := &recordingProvider{}
	reconciler := &HTTPRouteReconciler{
		Client: kubeClient, APIReader: baseClient, Provider: provider,
		Coordinator: &GraphCoordinator{}, Config: cfg,
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(route)}

	result, err := reconciler.Reconcile(context.Background(), request)
	if !errors.Is(err, statusErr) {
		t.Fatalf("first Reconcile() error = %v, want status patch failure", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("first Reconcile() result = %#v, want zero after patch failure", result)
	}
	if len(provider.deletedRoutes) != 0 {
		t.Fatalf("DeleteRoute calls before status checkpoint = %d, want 0", len(provider.deletedRoutes))
	}
	current := &gatewayv1.HTTPRoute{}
	if err := baseClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(current, cfg.routeFinalizer()) || len(current.Status.Parents) != 1 {
		t.Fatalf("failed checkpoint changed HTTPRoute: finalizers %#v, status %#v", current.Finalizers, current.Status)
	}

	kubeClient.patchErr = nil
	result, err = reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if result != (ctrl.Result{Requeue: true}) {
		t.Fatalf("second Reconcile() result = %#v, want status checkpoint", result)
	}
	if len(provider.deletedRoutes) != 0 {
		t.Fatalf("DeleteRoute calls while publishing status = %d, want 0", len(provider.deletedRoutes))
	}
	if err := baseClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	if len(current.Status.Parents) != 0 {
		t.Fatalf("HTTPRoute status after checkpoint = %#v, want no controller parents", current.Status.Parents)
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("third Reconcile() error = %v", err)
	}
	if len(provider.deletedRoutes) != 1 {
		t.Fatalf("DeleteRoute calls after status checkpoint = %d, want 1", len(provider.deletedRoutes))
	}
	if err := baseClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(current, cfg.routeFinalizer()) || routeHasBindingAnnotations(cfg, current) {
		t.Fatalf("detached HTTPRoute retained binding: finalizers %#v, annotations %#v", current.Finalizers, current.Annotations)
	}
}

func TestHTTPRouteProviderWarningWaitsForStatusPatch(t *testing.T) {
	cfg := testConfig()
	parent := gatewayv1.ParentReference{Name: "edge"}
	class, gateway := providerRouteParentObjects(cfg)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "api", UID: "route-uid", Generation: 1,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{parent}},
		},
	}
	(&HTTPRouteReconciler{Config: cfg}).applyRouteBinding(route, gateway)
	invalidStatus := statusForRouteBuildError(newRouteBuildError(
		routeErrorUnsupported,
		"Phase 1 supports exactly one HTTPRoute rule",
	))
	updates := []parentStatusUpdate{{parent: parent, status: invalidStatus}}
	applyRouteParentStatuses(cfg, route, updates)
	baseClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(route).
		WithObjects(class, gateway, route).
		Build()
	statusErr := errors.New("forced provider status patch failure")
	kubeClient := &patchFailingStatusClient{Client: baseClient, patchErr: statusErr}
	providerErr := cloud.NewProviderError(
		cloud.ErrorCategoryOwnershipConflict,
		errors.New("response=private-cloud-identifier"),
	)
	provider := &recordingProvider{routeDeleteErr: providerErr}
	recorder := events.NewFakeRecorder(2)
	reconciler := &HTTPRouteReconciler{
		Client: kubeClient, APIReader: baseClient, Provider: provider,
		Coordinator: &GraphCoordinator{}, Recorder: recorder, Config: cfg,
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(route)}

	result, err := reconciler.Reconcile(context.Background(), request)
	if !errors.Is(err, providerErr) || !errors.Is(err, statusErr) {
		t.Fatalf("first Reconcile() error = %v, want provider and status failures", err)
	}
	if strings.Contains(err.Error(), "private-cloud-identifier") {
		t.Fatalf("first Reconcile() exposed provider details: %q", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("first Reconcile() result = %#v, want zero after patch failure", result)
	}
	if len(provider.deletedRoutes) != 1 {
		t.Fatalf("DeleteRoute calls = %d, want 1", len(provider.deletedRoutes))
	}
	assertNoEvent(t, recorder)
	current := &gatewayv1.HTTPRoute{}
	if err := baseClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	if current.Annotations[cfg.routeCleanupFailureAnnotation()] != providerReasonOwnershipConflict {
		t.Fatalf("cleanup checkpoint = %q", current.Annotations[cfg.routeCleanupFailureAnnotation()])
	}
	programmed := findRouteParentCondition(current, cfg.ControllerName, parent, cfg.domain()+"/Programmed")
	if programmed == nil || programmed.Reason == providerReasonOwnershipConflict {
		t.Fatalf("provider condition was written despite patch failure: %#v", programmed)
	}

	kubeClient.patchErr = nil
	result, err = reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	assertJitteredRequeue(t, result.RequeueAfter, ownershipConflictRequeue)
	if event := receiveEvent(t, recorder); !strings.Contains(event, "OwnershipConflict") || strings.Contains(event, "private-cloud-identifier") {
		t.Fatalf("Event = %q", event)
	}
	if err := baseClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	programmed = findRouteParentCondition(current, cfg.ControllerName, parent, cfg.domain()+"/Programmed")
	if programmed == nil || programmed.Reason != providerReasonOwnershipConflict {
		t.Fatalf("provider condition after retry = %#v", programmed)
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("third Reconcile() error = %v", err)
	}
	assertNoEvent(t, recorder)
}

func TestHTTPRouteDetachReplacesPartialProviderFailureCheckpoint(t *testing.T) {
	cfg := testConfig()
	parent := gatewayv1.ParentReference{Name: "edge"}
	class, gateway := providerRouteParentObjects(cfg)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "api", UID: "route-uid", Generation: 1,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{parent}},
		},
	}
	(&HTTPRouteReconciler{Config: cfg}).applyRouteBinding(route, gateway)
	providerErr := cloud.NewProviderError(
		cloud.ErrorCategoryOwnershipConflict,
		errors.New("ownership conflict"),
	)
	policy, ok := providerFailurePolicyFor(providerErr)
	if !ok {
		t.Fatal("ownership conflict did not produce a provider failure policy")
	}
	policy = providerCleanupFailurePolicy(policy, "HTTPRoute")
	applyRouteProviderFailureStatuses(
		cfg,
		route,
		[]parentStatusUpdate{{
			parent: parent,
			status: providerFailureRouteStatus(policy, true),
		}},
		policy,
		true,
	)
	route.Status.Parents = append(route.Status.Parents, gatewayv1.RouteParentStatus{
		ParentRef:      gatewayv1.ParentReference{Name: "stale-gateway"},
		ControllerName: cfg.ControllerName,
		Conditions: []metav1.Condition{
			condition(cfg.domain()+"/Programmed", metav1.ConditionTrue, "Programmed", "Stale graph is programmed", 1),
		},
	})
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(route).
		WithObjects(class, gateway, route).
		Build()
	provider := &recordingProvider{}
	reconciler := &HTTPRouteReconciler{
		Client: kubeClient, APIReader: kubeClient, Provider: provider,
		Coordinator: &GraphCoordinator{}, Config: cfg,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(route),
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result != (ctrl.Result{Requeue: true}) {
		t.Fatalf("Reconcile() result = %#v, want replacement status checkpoint", result)
	}
	if len(provider.deletedRoutes) != 0 {
		t.Fatalf("DeleteRoute calls with partial status checkpoint = %d, want 0", len(provider.deletedRoutes))
	}
	current := &gatewayv1.HTTPRoute{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	if len(current.Status.Parents) != 1 ||
		!parentRefsEqual(current.Status.Parents[0].ParentRef, current.Namespace, parent, current.Namespace) {
		t.Fatalf("HTTPRoute parent statuses after checkpoint = %#v", current.Status.Parents)
	}
	programmed := findCondition(current.Status.Parents[0].Conditions, cfg.domain()+"/Programmed")
	if programmed == nil || programmed.Status != metav1.ConditionFalse || programmed.Reason != "Invalid" {
		t.Fatalf("Programmed condition after checkpoint = %#v", programmed)
	}
}

func TestHTTPRouteDeferredStatusRejectsControllerHandoff(t *testing.T) {
	cfg := testConfig()
	parent := gatewayv1.ParentReference{Name: "edge"}
	class, gateway := providerRouteParentObjects(cfg)
	foreignClass := class.DeepCopy()
	foreignClass.Spec.ControllerName = "other.example/controller"
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "api", UID: "route-uid", Generation: 1,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{parent}},
		},
	}
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(route).
		WithObjects(class, gateway, route).
		Build()
	liveReader := &gatewayClassFlipReader{
		Client: kubeClient, className: class.Name, afterFirst: foreignClass,
	}
	provider := &recordingProvider{}
	reconciler := &HTTPRouteReconciler{
		Client: kubeClient, APIReader: liveReader, Provider: provider,
		Coordinator: &GraphCoordinator{}, Config: cfg,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(route),
	})
	if !errors.Is(err, errHTTPRouteChanged) {
		t.Fatalf("Reconcile() error = %v, want controller handoff", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("Reconcile() result = %#v, want zero after deferred patch failure", result)
	}
	if len(provider.routeSpecs) != 0 || len(provider.deletedRoutes) != 0 {
		t.Fatalf("provider calls = ensures %d, deletes %d; want none", len(provider.routeSpecs), len(provider.deletedRoutes))
	}
	current := &gatewayv1.HTTPRoute{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	if len(current.Status.Parents) != 0 {
		t.Fatalf("status written after controller handoff: %#v", current.Status.Parents)
	}
}

func TestHTTPRouteBindingPatchFailurePreventsProviderMutation(t *testing.T) {
	cfg := testConfig()
	class, gateway := providerRouteParentObjects(cfg)
	route := mapperTestHTTPRoute(
		"default",
		"api",
		gatewayv1.ParentReference{Name: "edge"},
		"backend",
	)
	route.UID = "route-uid"
	route.Generation = 1
	service, endpointSlice, node := validNodePortBackend("default", "backend")
	baseClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(route).
		WithObjects(class, gateway, route, service, endpointSlice, node).
		Build()
	patchErr := errors.New("forced HTTPRoute binding patch failure")
	kubeClient := &patchFailingClient{Client: baseClient, err: patchErr}
	provider := &recordingProvider{}
	reconciler := &HTTPRouteReconciler{
		Client: kubeClient, APIReader: baseClient, Provider: provider,
		Coordinator: &GraphCoordinator{}, Config: cfg,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(route),
	})
	if !errors.Is(err, patchErr) {
		t.Fatalf("Reconcile() error = %v, want binding patch failure", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("Reconcile() result = %#v, want zero", result)
	}
	if len(provider.routeSpecs) != 0 {
		t.Fatalf("EnsureRoute calls before binding checkpoint = %d, want 0", len(provider.routeSpecs))
	}
	current := &gatewayv1.HTTPRoute{}
	if err := baseClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(current, cfg.routeFinalizer()) || routeHasBindingAnnotations(cfg, current) {
		t.Fatalf("failed binding patch changed HTTPRoute: finalizers %#v, annotations %#v", current.Finalizers, current.Annotations)
	}
}

func TestHTTPRouteCleanupCheckpointFailureSuppressesWarning(t *testing.T) {
	cfg := testConfig()
	route, _ := deletingBoundRoute(cfg)
	baseClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(route).
		WithObjects(route).
		Build()
	checkpointErr := errors.New("forced cleanup checkpoint failure")
	kubeClient := &patchFailingClient{Client: baseClient, err: checkpointErr}
	providerErr := cloud.NewProviderError(
		cloud.ErrorCategoryOwnershipConflict,
		errors.New("response=private-cloud-identifier"),
	)
	provider := &recordingProvider{routeDeleteErr: providerErr}
	recorder := events.NewFakeRecorder(1)
	reconciler := &HTTPRouteReconciler{
		Client: kubeClient, APIReader: baseClient, Provider: provider,
		Coordinator: &GraphCoordinator{}, Recorder: recorder, Config: cfg,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(route),
	})
	if !errors.Is(err, providerErr) || !errors.Is(err, checkpointErr) {
		t.Fatalf("Reconcile() error = %v, want provider and checkpoint failures", err)
	}
	if strings.Contains(err.Error(), "private-cloud-identifier") {
		t.Fatalf("Reconcile() exposed provider details: %q", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("Reconcile() result = %#v, want zero", result)
	}
	if len(provider.deletedRoutes) != 1 {
		t.Fatalf("DeleteRoute calls = %d, want 1", len(provider.deletedRoutes))
	}
	assertNoEvent(t, recorder)
	current := &gatewayv1.HTTPRoute{}
	if err := baseClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	if current.Annotations[cfg.routeCleanupFailureAnnotation()] != "" {
		t.Fatalf("cleanup annotation was written despite patch failure: %#v", current.Annotations)
	}
	if !controllerutil.ContainsFinalizer(current, cfg.routeFinalizer()) || !routeHasBindingAnnotations(cfg, current) {
		t.Fatalf("cleanup checkpoint failure removed binding: finalizers %#v, annotations %#v", current.Finalizers, current.Annotations)
	}
}

func TestHTTPRouteReconcilePreservesBindingWhenCanceled(t *testing.T) {
	t.Run("normal detach status checkpoint", func(t *testing.T) {
		cfg := testConfig()
		route := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default", Name: "api", UID: "route-uid", Generation: 1,
				Finalizers: routeBindingFinalizers(cfg, "default", "old-gateway", "old-gateway-uid"),
				Annotations: map[string]string{
					cfg.routeGatewayNamespaceAnnotation(): "default",
					cfg.routeGatewayNameAnnotation():      "old-gateway",
					cfg.routeGatewayUIDAnnotation():       "old-gateway-uid",
				},
			},
			Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{{
				ParentRef:      gatewayv1.ParentReference{Name: "old-gateway"},
				ControllerName: cfg.ControllerName,
			}}}},
		}
		baseClient := indexedFakeClientBuilder(testScheme(t), cfg).
			WithStatusSubresource(route).
			WithObjects(route).
			Build()
		ctx, cancel := context.WithCancel(context.Background())
		reader := &cancelingHTTPRouteReader{
			Reader: baseClient, key: client.ObjectKeyFromObject(route), cancel: cancel,
		}
		provider := &recordingProvider{}
		recorder := events.NewFakeRecorder(1)
		reconciler := &HTTPRouteReconciler{
			Client: baseClient, APIReader: reader, Provider: provider,
			Coordinator: &GraphCoordinator{}, Recorder: recorder, Config: cfg,
		}

		result, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: client.ObjectKeyFromObject(route),
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Reconcile() error = %v, want context cancellation", err)
		}
		if result != (ctrl.Result{}) || len(provider.deletedRoutes) != 0 {
			t.Fatalf("Reconcile() result = %#v, DeleteRoute calls = %d", result, len(provider.deletedRoutes))
		}
		assertRouteBindingPreserved(t, baseClient, cfg, route)
		assertNoEvent(t, recorder)
	})

	t.Run("deletion binding validation", func(t *testing.T) {
		cfg := testConfig()
		route, _ := deletingBoundRoute(cfg)
		baseClient := indexedFakeClientBuilder(testScheme(t), cfg).
			WithStatusSubresource(route).
			WithObjects(route).
			Build()
		ctx, cancel := context.WithCancel(context.Background())
		reader := &cancelingHTTPRouteReader{
			Reader: baseClient, key: client.ObjectKeyFromObject(route), cancel: cancel,
		}
		provider := &recordingProvider{}
		recorder := events.NewFakeRecorder(1)
		reconciler := &HTTPRouteReconciler{
			Client: baseClient, APIReader: reader, Provider: provider,
			Coordinator: &GraphCoordinator{}, Recorder: recorder, Config: cfg,
		}

		result, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: client.ObjectKeyFromObject(route),
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Reconcile() error = %v, want context cancellation", err)
		}
		if result != (ctrl.Result{}) || len(provider.deletedRoutes) != 0 {
			t.Fatalf("Reconcile() result = %#v, DeleteRoute calls = %d", result, len(provider.deletedRoutes))
		}
		assertRouteBindingPreserved(t, baseClient, cfg, route)
		assertNoEvent(t, recorder)
	})
}

func assertRouteBindingPreserved(
	t *testing.T,
	kubeClient client.Client,
	cfg Config,
	route *gatewayv1.HTTPRoute,
) {
	t.Helper()
	current := &gatewayv1.HTTPRoute{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(current, cfg.routeFinalizer()) || !routeHasBindingAnnotations(cfg, current) {
		t.Fatalf("HTTPRoute binding was not preserved: finalizers %#v, annotations %#v", current.Finalizers, current.Annotations)
	}
}

func TestApplyRouteParentStatusesKeepsRequiredEmptyArray(t *testing.T) {
	route := &gatewayv1.HTTPRoute{}
	applyRouteParentStatuses(testConfig(), route, nil)
	if route.Status.Parents == nil {
		t.Fatal("status.parents is nil, want the required empty array")
	}
}

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for index := range conditions {
		if conditions[index].Type == conditionType {
			return &conditions[index]
		}
	}
	return nil
}

func findRouteParentCondition(
	route *gatewayv1.HTTPRoute,
	controllerName gatewayv1.GatewayController,
	parent gatewayv1.ParentReference,
	conditionType string,
) *metav1.Condition {
	for index := range route.Status.Parents {
		status := &route.Status.Parents[index]
		if status.ControllerName == controllerName &&
			parentRefsEqual(status.ParentRef, route.Namespace, parent, route.Namespace) {
			return findCondition(status.Conditions, conditionType)
		}
	}
	return nil
}

type conflictOnceRouteStatusClient struct {
	client.Client
	mutate     func(*gatewayv1.HTTPRoute)
	patchCalls int
}

type patchFailingClient struct {
	client.Client
	err error
}

func (c *patchFailingClient) Patch(
	context.Context,
	client.Object,
	client.Patch,
	...client.PatchOption,
) error {
	return c.err
}

type cancelingHTTPRouteReader struct {
	client.Reader
	key    client.ObjectKey
	cancel context.CancelFunc
}

func (r *cancelingHTTPRouteReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	if _, ok := object.(*gatewayv1.HTTPRoute); ok && key == r.key {
		r.cancel()
		return ctx.Err()
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func (c *conflictOnceRouteStatusClient) Status() client.StatusWriter {
	return &conflictOnceRouteStatusWriter{
		SubResourceWriter: c.Client.Status(),
		client:            c,
	}
}

type conflictOnceRouteStatusWriter struct {
	client.SubResourceWriter
	client *conflictOnceRouteStatusClient
}

func (w *conflictOnceRouteStatusWriter) Patch(
	ctx context.Context,
	object client.Object,
	patch client.Patch,
	options ...client.SubResourcePatchOption,
) error {
	w.client.patchCalls++
	if w.client.patchCalls == 1 {
		current := &gatewayv1.HTTPRoute{}
		if err := w.client.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil {
			return err
		}
		w.client.mutate(current)
		if err := w.client.Client.Status().Update(ctx, current); err != nil {
			return err
		}
		return apierrors.NewConflict(
			schema.GroupResource{Group: gatewayv1.GroupName, Resource: "httproutes"},
			object.GetName(),
			errors.New("injected status conflict"),
		)
	}
	return w.SubResourceWriter.Patch(ctx, object, patch, options...)
}
