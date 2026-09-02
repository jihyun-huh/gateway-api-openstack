//go:build e2e

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
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestDeleteMarkedAndWaitUsesRunMarkerAndUID(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	routeUID := types.UID("route-uid")
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Namespace:   "e2e",
		Name:        routeName,
		UID:         routeUID,
		Annotations: map[string]string{runAnnotation: "run-1234"},
	}}
	suite := &phase2Suite{
		client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(route).Build(),
		config: e2eConfig{RunID: "run-1234", PollInterval: time.Millisecond},
	}
	object := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: "e2e", Name: routeName}}
	if err := suite.deleteMarkedAndWait(context.Background(), object, &routeUID); err != nil {
		t.Fatalf("deleteMarkedAndWait() error = %v", err)
	}
	if err := suite.client.Get(context.Background(), client.ObjectKeyFromObject(route), &gatewayv1.HTTPRoute{}); !apierrors.IsNotFound(err) {
		t.Fatalf("deleted HTTPRoute Get() error = %v, want NotFound", err)
	}
}

func TestDeleteMarkedAndWaitRefusesForeignObject(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Namespace:   "e2e",
		Name:        routeName,
		UID:         types.UID("foreign-uid"),
		Annotations: map[string]string{runAnnotation: "different-run"},
	}}
	suite := &phase2Suite{
		client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(route).Build(),
		config: e2eConfig{RunID: "run-1234", PollInterval: time.Millisecond},
	}
	expectedUID := types.UID("route-uid")
	object := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: "e2e", Name: routeName}}
	if err := suite.deleteMarkedAndWait(context.Background(), object, &expectedUID); err == nil {
		t.Fatal("deleteMarkedAndWait() deleted or accepted a foreign object")
	}
	if err := suite.client.Get(context.Background(), client.ObjectKeyFromObject(route), &gatewayv1.HTTPRoute{}); err != nil {
		t.Fatalf("foreign HTTPRoute was changed: %v", err)
	}
}

func TestEnsureCleanupStopsAtFirstDependencyFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	classUID := types.UID("class-uid")
	namespaceUID := types.UID("namespace-uid")
	gatewayClass := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{
		Name:        "gao-e2e-run-1234",
		UID:         classUID,
		Annotations: map[string]string{runAnnotation: "different-run"},
	}}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        "gateway-api-openstack-e2e-run-1234",
		UID:         namespaceUID,
		Annotations: map[string]string{runAnnotation: "run-1234"},
	}}
	suite := &phase2Suite{
		client:           fake.NewClientBuilder().WithScheme(scheme).WithObjects(gatewayClass, namespace).Build(),
		config:           e2eConfig{RunID: "run-1234", Namespace: namespace.Name, PollInterval: time.Millisecond},
		gatewayClassName: gatewayClass.Name,
		createdClass:     true,
		createdNamespace: true,
		gatewayClassUID:  classUID,
		namespaceUID:     namespaceUID,
	}
	if err := suite.ensureCleanup(context.Background()); err == nil {
		t.Fatal("ensureCleanup() accepted a foreign GatewayClass")
	}
	if err := suite.client.Get(context.Background(), client.ObjectKeyFromObject(namespace), &corev1.Namespace{}); err != nil {
		t.Fatalf("ensureCleanup() continued past the failed dependency: %v", err)
	}
}
