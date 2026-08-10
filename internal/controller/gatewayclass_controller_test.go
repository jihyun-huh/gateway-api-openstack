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
	"reflect"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayconsts "sigs.k8s.io/gateway-api/pkg/consts"
)

func TestGatewayClassReconcileAcceptsWithoutOverstatingConformance(t *testing.T) {
	customCondition := metav1.Condition{
		Type:               "example.net/ExternalCondition",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 1,
		Reason:             "Configured",
		Message:            "set by another component",
		LastTransitionTime: metav1.Now(),
	}
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 3},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController("example.com/gateway-api-openstack"),
		},
		Status: gatewayv1.GatewayClassStatus{Conditions: []metav1.Condition{customCondition}},
	}
	reconciler, kubeClient := newGatewayClassReconciler(t, gatewayClass)
	var before gatewayv1.GatewayClass
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Name: gatewayClass.Name}, &before); err != nil {
		t.Fatal(err)
	}
	customBefore := meta.FindStatusCondition(before.Status.Conditions, customCondition.Type)
	if customBefore == nil {
		t.Fatal("custom condition missing before reconciliation")
	}

	reconcileGatewayClass(t, reconciler, gatewayClass.Name)

	var got gatewayv1.GatewayClass
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Name: gatewayClass.Name}, &got); err != nil {
		t.Fatal(err)
	}
	accepted := meta.FindStatusCondition(got.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionTrue || accepted.Reason != string(gatewayv1.GatewayClassReasonAccepted) {
		t.Fatalf("Accepted condition = %#v", accepted)
	}
	if accepted.ObservedGeneration != gatewayClass.Generation {
		t.Fatalf("Accepted observedGeneration = %d, want %d", accepted.ObservedGeneration, gatewayClass.Generation)
	}
	supportedVersion := meta.FindStatusCondition(got.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion))
	if supportedVersion == nil || supportedVersion.Status != metav1.ConditionTrue ||
		supportedVersion.Reason != string(gatewayv1.GatewayClassReasonSupportedVersion) {
		t.Fatalf("SupportedVersion condition = %#v", supportedVersion)
	}
	if supportedVersion.ObservedGeneration != gatewayClass.Generation {
		t.Fatalf("SupportedVersion observedGeneration = %d, want %d", supportedVersion.ObservedGeneration, gatewayClass.Generation)
	}
	if custom := meta.FindStatusCondition(got.Status.Conditions, customCondition.Type); custom == nil || !reflect.DeepEqual(*custom, *customBefore) {
		t.Fatalf("custom condition was not preserved: %#v", custom)
	}
	var wantFeatures []gatewayv1.SupportedFeature
	if !reflect.DeepEqual(got.Status.SupportedFeatures, wantFeatures) {
		t.Fatalf("supportedFeatures = %#v, want %#v", got.Status.SupportedFeatures, wantFeatures)
	}

	beforeSecondReconcile := got.DeepCopy()
	reconcileGatewayClass(t, reconciler, gatewayClass.Name)
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Name: gatewayClass.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeSecondReconcile, &got) {
		t.Fatalf("second reconciliation changed a converged GatewayClass:\nbefore: %#v\nafter:  %#v", beforeSecondReconcile, &got)
	}
}

func TestGatewayClassReconcileRestoresOwnedConditions(t *testing.T) {
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 1},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController("example.com/gateway-api-openstack"),
		},
	}
	reconciler, kubeClient := newGatewayClassReconciler(t, gatewayClass)
	reconcileGatewayClass(t, reconciler, gatewayClass.Name)

	var drifted gatewayv1.GatewayClass
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Name: gatewayClass.Name}, &drifted); err != nil {
		t.Fatal(err)
	}
	drifted.Status.Conditions = nil
	if err := kubeClient.Status().Update(context.Background(), &drifted); err != nil {
		t.Fatal(err)
	}
	reconcileGatewayClass(t, reconciler, gatewayClass.Name)

	var got gatewayv1.GatewayClass
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Name: gatewayClass.Name}, &got); err != nil {
		t.Fatal(err)
	}
	for _, conditionType := range []string{
		string(gatewayv1.GatewayClassConditionStatusAccepted),
		string(gatewayv1.GatewayClassConditionStatusSupportedVersion),
	} {
		condition := meta.FindStatusCondition(got.Status.Conditions, conditionType)
		if condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != gatewayClass.Generation {
			t.Fatalf("%s condition = %#v", conditionType, condition)
		}
	}
}

func TestGatewayClassReconcileRejectsParametersRef(t *testing.T) {
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 2},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController("example.com/gateway-api-openstack"),
			ParametersRef: &gatewayv1.ParametersReference{
				Group: "example.com",
				Kind:  "OpenStackGatewayClassConfig",
				Name:  "default",
			},
		},
		Status: gatewayv1.GatewayClassStatus{
			SupportedFeatures: []gatewayv1.SupportedFeature{{Name: "Gateway"}},
		},
	}
	reconciler, kubeClient := newGatewayClassReconciler(t, gatewayClass)

	reconcileGatewayClass(t, reconciler, gatewayClass.Name)

	var got gatewayv1.GatewayClass
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Name: gatewayClass.Name}, &got); err != nil {
		t.Fatal(err)
	}
	accepted := meta.FindStatusCondition(got.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionFalse || accepted.Reason != string(gatewayv1.GatewayClassReasonInvalidParameters) {
		t.Fatalf("Accepted condition = %#v", accepted)
	}
	if len(got.Status.SupportedFeatures) != 0 {
		t.Fatalf("supportedFeatures = %#v, want none", got.Status.SupportedFeatures)
	}
}

func TestGatewayClassReconcileReportsUnsupportedGatewayAPIVersion(t *testing.T) {
	experimentalMismatch := gatewayAPICRD("xbackendtrafficpolicies.gateway.networking.x-k8s.io", "v2.0.0")
	experimentalMismatch.Spec.Group = gatewayAPIExperimentalGroup
	terminatingRouteCRD := gatewayAPICRD("httproutes.gateway.networking.k8s.io", gatewayconsts.BundleVersion)
	now := metav1.Now()
	terminatingRouteCRD.DeletionTimestamp = &now
	terminatingRouteCRD.Finalizers = []string{"test.example/finalizer"}
	tests := []struct {
		name        string
		definitions []client.Object
		wantMessage string
	}{
		{
			name: "required CRD is missing",
			definitions: []client.Object{
				gatewayAPICRD("gatewayclasses.gateway.networking.k8s.io", gatewayconsts.BundleVersion),
				gatewayAPICRD("gateways.gateway.networking.k8s.io", gatewayconsts.BundleVersion),
			},
			wantMessage: "httproutes.gateway.networking.k8s.io is not installed",
		},
		{
			name: "annotation is missing",
			definitions: []client.Object{
				gatewayAPICRD("gatewayclasses.gateway.networking.k8s.io", gatewayconsts.BundleVersion),
				gatewayAPICRD("gateways.gateway.networking.k8s.io", gatewayconsts.BundleVersion),
				gatewayAPICRD("httproutes.gateway.networking.k8s.io", ""),
			},
			wantMessage: "httproutes.gateway.networking.k8s.io has no bundle version annotation",
		},
		{
			name: "required CRDs use mixed versions",
			definitions: []client.Object{
				gatewayAPICRD("gatewayclasses.gateway.networking.k8s.io", gatewayconsts.BundleVersion),
				gatewayAPICRD("gateways.gateway.networking.k8s.io", "v1.5.0"),
				gatewayAPICRD("httproutes.gateway.networking.k8s.io", gatewayconsts.BundleVersion),
			},
			wantMessage: "gateways.gateway.networking.k8s.io uses bundle version v1.5.0",
		},
		{
			name: "another Gateway API CRD uses an unknown version",
			definitions: append(
				supportedGatewayAPICRDs(),
				gatewayAPICRD("grpcroutes.gateway.networking.k8s.io", "v2.0.0"),
			),
			wantMessage: "grpcroutes.gateway.networking.k8s.io uses bundle version v2.0.0",
		},
		{
			name: "experimental Gateway API CRD uses an unknown version",
			definitions: append(
				supportedGatewayAPICRDs(),
				experimentalMismatch,
			),
			wantMessage: "xbackendtrafficpolicies.gateway.networking.x-k8s.io uses bundle version v2.0.0",
		},
		{
			name: "required CRD is being deleted",
			definitions: []client.Object{
				gatewayAPICRD("gatewayclasses.gateway.networking.k8s.io", gatewayconsts.BundleVersion),
				gatewayAPICRD("gateways.gateway.networking.k8s.io", gatewayconsts.BundleVersion),
				terminatingRouteCRD,
			},
			wantMessage: "httproutes.gateway.networking.k8s.io is being deleted",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gatewayClass := &gatewayv1.GatewayClass{
				ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 4},
				Spec: gatewayv1.GatewayClassSpec{
					ControllerName: gatewayv1.GatewayController("example.com/gateway-api-openstack"),
				},
			}
			reconciler, kubeClient := newGatewayClassReconcilerWithCRDs(t, test.definitions, gatewayClass)

			reconcileGatewayClass(t, reconciler, gatewayClass.Name)

			var got gatewayv1.GatewayClass
			if err := kubeClient.Get(context.Background(), types.NamespacedName{Name: gatewayClass.Name}, &got); err != nil {
				t.Fatal(err)
			}
			supportedVersion := meta.FindStatusCondition(got.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion))
			if supportedVersion == nil || supportedVersion.Status != metav1.ConditionFalse ||
				supportedVersion.Reason != string(gatewayv1.GatewayClassReasonUnsupportedVersion) {
				t.Fatalf("SupportedVersion condition = %#v", supportedVersion)
			}
			if supportedVersion.ObservedGeneration != gatewayClass.Generation {
				t.Fatalf("SupportedVersion observedGeneration = %d, want %d", supportedVersion.ObservedGeneration, gatewayClass.Generation)
			}
			if !strings.Contains(supportedVersion.Message, test.wantMessage) {
				t.Fatalf("SupportedVersion message = %q, want it to contain %q", supportedVersion.Message, test.wantMessage)
			}
			accepted := meta.FindStatusCondition(got.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
			if accepted == nil || accepted.Status != metav1.ConditionFalse ||
				accepted.Reason != string(gatewayv1.GatewayClassReasonUnsupportedVersion) {
				t.Fatalf("Accepted condition = %#v", accepted)
			}
			if len(got.Status.SupportedFeatures) != 0 {
				t.Fatalf("supportedFeatures = %#v, want none", got.Status.SupportedFeatures)
			}
		})
	}
}

func TestGatewayClassUnsupportedVersionRequeuesAndRecoversWithoutWatch(t *testing.T) {
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", UID: "gateway-class-uid", Generation: 1},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController("example.com/gateway-api-openstack"),
		},
	}
	reconciler, kubeClient := newGatewayClassReconcilerWithCRDs(
		t,
		unsupportedGatewayAPICRDs(),
		gatewayClass,
	)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: gatewayClass.Name},
	})
	if err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("unsupported GatewayClass did not schedule a version recheck")
	}
	var routeDefinition apiextensionsv1.CustomResourceDefinition
	if err := kubeClient.Get(
		context.Background(),
		types.NamespacedName{Name: "httproutes.gateway.networking.k8s.io"},
		&routeDefinition,
	); err != nil {
		t.Fatal(err)
	}
	routeDefinition.Annotations[gatewayconsts.BundleVersionAnnotation] = gatewayconsts.BundleVersion
	if err := kubeClient.Update(context.Background(), &routeDefinition); err != nil {
		t.Fatal(err)
	}

	result, err = reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: gatewayClass.Name},
	})
	if err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("supported GatewayClass RequeueAfter = %s, want zero", result.RequeueAfter)
	}
	var got gatewayv1.GatewayClass
	if err := kubeClient.Get(
		context.Background(),
		types.NamespacedName{Name: gatewayClass.Name},
		&got,
	); err != nil {
		t.Fatal(err)
	}
	supportedVersion := meta.FindStatusCondition(
		got.Status.Conditions,
		string(gatewayv1.GatewayClassConditionStatusSupportedVersion),
	)
	if supportedVersion == nil || supportedVersion.Status != metav1.ConditionTrue {
		t.Fatalf("SupportedVersion condition = %#v, want True", supportedVersion)
	}
}

func TestGatewayClassVersionMismatchTakesPrecedenceOverParametersRef(t *testing.T) {
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 2},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController("example.com/gateway-api-openstack"),
			ParametersRef: &gatewayv1.ParametersReference{
				Group: "example.com",
				Kind:  "OpenStackGatewayClassConfig",
				Name:  "default",
			},
		},
	}
	reconciler, kubeClient := newGatewayClassReconcilerWithCRDs(t, unsupportedGatewayAPICRDs(), gatewayClass)

	reconcileGatewayClass(t, reconciler, gatewayClass.Name)

	var got gatewayv1.GatewayClass
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Name: gatewayClass.Name}, &got); err != nil {
		t.Fatal(err)
	}
	accepted := meta.FindStatusCondition(got.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionFalse ||
		accepted.Reason != string(gatewayv1.GatewayClassReasonUnsupportedVersion) {
		t.Fatalf("Accepted condition = %#v", accepted)
	}
}

func TestGatewayClassReconcilePreservesStatusWhenCRDListFails(t *testing.T) {
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 3},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController("example.com/gateway-api-openstack"),
		},
		Status: gatewayv1.GatewayClassStatus{Conditions: []metav1.Condition{{
			Type:               "example.net/ExternalCondition",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: 2,
			Reason:             "Configured",
			Message:            "set by another component",
			LastTransitionTime: metav1.Now(),
		}}},
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gatewayClass.Name),
		},
	}
	reconciler, kubeClient := newGatewayClassReconciler(t, gatewayClass, gateway)
	wantErr := errors.New("CRD discovery is unavailable")
	reconciler.APIReader = &listFailingReader{Reader: kubeClient, err: wantErr}
	var before gatewayv1.GatewayClass
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Name: gatewayClass.Name}, &before); err != nil {
		t.Fatal(err)
	}

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: gatewayClass.Name}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("finalizer checkpoint Reconcile() error = %v", err)
	}
	var checkpoint gatewayv1.GatewayClass
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Name: gatewayClass.Name}, &checkpoint); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(&checkpoint, gatewayv1.GatewayClassFinalizerGatewaysExist) {
		t.Fatal("GatewaysExist finalizer was not persisted before CRD discovery")
	}
	if !reflect.DeepEqual(before.Status, checkpoint.Status) {
		t.Fatalf("GatewayClass status changed at the finalizer checkpoint:\nbefore: %#v\nafter:  %#v", before.Status, checkpoint.Status)
	}

	_, err := reconciler.Reconcile(context.Background(), request)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Reconcile() error = %v, want %v", err, wantErr)
	}
	var after gatewayv1.GatewayClass
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Name: gatewayClass.Name}, &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Status, after.Status) {
		t.Fatalf("GatewayClass status changed after CRD list failure:\nbefore: %#v\nafter:  %#v", before.Status, after.Status)
	}
}

func TestGatewayClassReconcileReturnsDeferredPatchError(t *testing.T) {
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack"},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController("example.com/gateway-api-openstack"),
		},
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gatewayClass.Name),
		},
	}
	reconciler, kubeClient := newGatewayClassReconciler(t, gatewayClass, gateway)
	patchErr := errors.New("GatewayClass patch is unavailable")
	reconciler.Client = &gatewayClassPatchFailingClient{Client: kubeClient, err: patchErr}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: gatewayClass.Name},
	})
	if !errors.Is(err, patchErr) {
		t.Fatalf("Reconcile() error = %v, want %v", err, patchErr)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("Reconcile() result = %#v, want zero after deferred patch failure", result)
	}
}

func TestGatewayClassFinalizerCheckpointSurvivesCanceledDiscovery(t *testing.T) {
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack"},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController("example.com/gateway-api-openstack"),
		},
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gatewayClass.Name),
		},
	}
	reconciler, kubeClient := newGatewayClassReconciler(t, gatewayClass, gateway)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reconciler.APIReader = &cancelingListReader{Reader: kubeClient, cancel: cancel}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: gatewayClass.Name}}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("finalizer checkpoint Reconcile() error = %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("status Reconcile() error = %v, want %v", err, context.Canceled)
	}
	var got gatewayv1.GatewayClass
	if err := kubeClient.Get(
		context.Background(),
		types.NamespacedName{Name: gatewayClass.Name},
		&got,
	); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(&got, gatewayv1.GatewayClassFinalizerGatewaysExist) {
		t.Fatal("GatewaysExist finalizer was lost when CRD discovery canceled the request")
	}
}

func TestGatewayClassReconcileConvergesGatewaysExistFinalizer(t *testing.T) {
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack"},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController("example.com/gateway-api-openstack"),
		},
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gatewayClass.Name),
		},
	}
	reconciler, kubeClient := newGatewayClassReconciler(t, gatewayClass, gateway)

	reconcileGatewayClass(t, reconciler, gatewayClass.Name)
	var got gatewayv1.GatewayClass
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Name: gatewayClass.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(&got, gatewayv1.GatewayClassFinalizerGatewaysExist) {
		t.Fatalf("finalizers = %#v, want %q", got.Finalizers, gatewayv1.GatewayClassFinalizerGatewaysExist)
	}

	if err := kubeClient.Delete(context.Background(), gateway); err != nil {
		t.Fatal(err)
	}
	reconcileGatewayClass(t, reconciler, gatewayClass.Name)
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Name: gatewayClass.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(&got, gatewayv1.GatewayClassFinalizerGatewaysExist) {
		t.Fatalf("finalizers = %#v, expected GatewaysExist finalizer to be removed", got.Finalizers)
	}
}

func TestGatewayClassReconcileIgnoresAnotherController(t *testing.T) {
	accepted := metav1.Condition{
		Type:               string(gatewayv1.GatewayClassConditionStatusAccepted),
		Status:             metav1.ConditionFalse,
		ObservedGeneration: 1,
		Reason:             string(gatewayv1.GatewayClassReasonUnsupported),
		Message:            "owned by another controller",
		LastTransitionTime: metav1.Now(),
	}
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "other",
			Finalizers: []string{"other.example/finalizer"},
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController("other.example/controller"),
		},
		Status: gatewayv1.GatewayClassStatus{
			Conditions:        []metav1.Condition{accepted},
			SupportedFeatures: []gatewayv1.SupportedFeature{{Name: "OtherFeature"}},
		},
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: gatewayv1.ObjectName(gatewayClass.Name)},
	}
	reconciler, kubeClient := newGatewayClassReconciler(t, gatewayClass, gateway)
	var before gatewayv1.GatewayClass
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Name: gatewayClass.Name}, &before); err != nil {
		t.Fatal(err)
	}

	reconcileGatewayClass(t, reconciler, gatewayClass.Name)

	var after gatewayv1.GatewayClass
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Name: gatewayClass.Name}, &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("unmanaged GatewayClass changed:\nbefore: %#v\nafter:  %#v", before, after)
	}
}

func TestGatewayClassRequestsForGateway(t *testing.T) {
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "openstack"},
	}
	requests := gatewayClassRequestsForGateway(context.Background(), gateway)
	want := []ctrl.Request{{NamespacedName: types.NamespacedName{Name: "openstack"}}}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests = %#v, want %#v", requests, want)
	}
	if requests := gatewayClassRequestsForGateway(context.Background(), &gatewayv1.GatewayClass{}); requests != nil {
		t.Fatalf("requests for non-Gateway object = %#v, want nil", requests)
	}
}

func TestGatewayClassRequestsForGatewayAPICRD(t *testing.T) {
	managed := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "z-openstack"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "example.com/gateway-api-openstack"},
	}
	managedFirst := managed.DeepCopy()
	managedFirst.Name = "a-openstack"
	unmanaged := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "other"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "other.example/controller"},
	}
	reconciler, _ := newGatewayClassReconciler(t, managed, managedFirst, unmanaged)
	definition := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
		Name: "gateways.gateway.networking.k8s.io",
	}}
	definition.SetGroupVersionKind(apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinition"))
	requests := reconciler.gatewayClassRequestsForGatewayAPICRD(
		context.Background(),
		definition,
	)
	want := []ctrl.Request{
		{NamespacedName: types.NamespacedName{Name: managedFirst.Name}},
		{NamespacedName: types.NamespacedName{Name: managed.Name}},
	}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests = %#v, want %#v", requests, want)
	}
	otherGroup := gatewayAPICRD("widgets.example.com", gatewayconsts.BundleVersion)
	otherGroup.Spec.Group = "example.com"
	if requests := reconciler.gatewayClassRequestsForGatewayAPICRD(context.Background(), otherGroup); requests != nil {
		t.Fatalf("requests for another API group = %#v, want nil", requests)
	}
	nestedGroup := gatewayAPICRD("widgets.foo.gateway.networking.k8s.io", gatewayconsts.BundleVersion)
	nestedGroup.Spec.Group = "foo.gateway.networking.k8s.io"
	if requests := reconciler.gatewayClassRequestsForGatewayAPICRD(context.Background(), nestedGroup); requests != nil {
		t.Fatalf("requests for a suffix-matching API group = %#v, want nil", requests)
	}
}

func newGatewayClassReconciler(t *testing.T, objects ...client.Object) (*GatewayClassReconciler, client.Client) {
	t.Helper()
	return newGatewayClassReconcilerWithCRDs(t, supportedGatewayAPICRDs(), objects...)
}

func newGatewayClassReconcilerWithCRDs(
	t *testing.T,
	definitions []client.Object,
	objects ...client.Object,
) (*GatewayClassReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	allObjects := append(append([]client.Object(nil), objects...), definitions...)
	kubeClient := rawIndexedFakeClientBuilder(scheme, testConfig()).
		WithStatusSubresource(&gatewayv1.GatewayClass{}).
		WithObjects(allObjects...).
		Build()
	reconciler := &GatewayClassReconciler{
		Client:    kubeClient,
		APIReader: kubeClient,
		Config:    Config{ControllerName: gatewayv1.GatewayController("example.com/gateway-api-openstack")},
	}
	return reconciler, kubeClient
}

func supportedGatewayAPICRDs() []client.Object {
	definitions := make([]client.Object, 0, len(requiredGatewayAPICRDs))
	for _, name := range requiredGatewayAPICRDs {
		definitions = append(definitions, gatewayAPICRD(name, gatewayconsts.BundleVersion))
	}
	return definitions
}

func gatewayAPICRD(name, bundleVersion string) *apiextensionsv1.CustomResourceDefinition {
	annotations := map[string]string{}
	if bundleVersion != "" {
		annotations[gatewayconsts.BundleVersionAnnotation] = bundleVersion
	}
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: gatewayv1.GroupName,
		},
	}
}

type listFailingReader struct {
	client.Reader
	err error
}

type cancelingListReader struct {
	client.Reader
	cancel context.CancelFunc
}

func (r *cancelingListReader) List(ctx context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	r.cancel()
	return ctx.Err()
}

func (r *listFailingReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return r.err
}

type gatewayClassPatchFailingClient struct {
	client.Client
	err error
}

func (c *gatewayClassPatchFailingClient) Patch(
	context.Context,
	client.Object,
	client.Patch,
	...client.PatchOption,
) error {
	return c.err
}

func reconcileGatewayClass(t *testing.T, reconciler *GatewayClassReconciler, name string) {
	t.Helper()
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
}
