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
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
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

func newGatewayClassReconciler(t *testing.T, objects ...client.Object) (*GatewayClassReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := indexedFakeClientBuilder(scheme, testConfig()).
		WithStatusSubresource(&gatewayv1.GatewayClass{}).
		WithObjects(objects...).
		Build()
	reconciler := &GatewayClassReconciler{
		Client: kubeClient,
		Config: Config{ControllerName: gatewayv1.GatewayController("example.com/gateway-api-openstack")},
	}
	return reconciler, kubeClient
}

func reconcileGatewayClass(t *testing.T, reconciler *GatewayClassReconciler, name string) {
	t.Helper()
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
}
