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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayconsts "sigs.k8s.io/gateway-api/pkg/consts"
)

func TestGatewayReconcilePredicate(t *testing.T) {
	cfg := testConfig()
	base := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{
		Namespace:   "default",
		Name:        "edge",
		Generation:  1,
		Annotations: map[string]string{"example.com/foreign": "one"},
		Finalizers:  []string{"example.com/foreign"},
	}}
	pred := GatewayReconcilePredicate(cfg)

	testPredicateUpdate(t, pred, base, mutateGateway(base, func(gateway *gatewayv1.Gateway) {
		gateway.Status.Addresses = []gatewayv1.GatewayStatusAddress{{Value: "192.0.2.10"}}
	}), false, "status only")
	testPredicateUpdate(t, pred, base, mutateGateway(base, func(gateway *gatewayv1.Gateway) {
		gateway.Annotations["example.com/foreign"] = "two"
	}), false, "foreign annotation")
	testPredicateUpdate(t, pred, base, mutateGateway(base, func(gateway *gatewayv1.Gateway) {
		gateway.Annotations[cfg.GatewayListenerPortAnnotation()] = "80"
	}), true, "binding annotation")
	testPredicateUpdate(t, pred, base, mutateGateway(base, func(gateway *gatewayv1.Gateway) {
		gateway.Finalizers = append(gateway.Finalizers, cfg.GatewayFinalizer())
	}), true, "controller finalizer")
	testPredicateUpdate(t, pred, base, mutateGateway(base, func(gateway *gatewayv1.Gateway) {
		gateway.Generation++
	}), true, "generation")
	testPredicateUpdate(t, pred, base, mutateGateway(base, markGatewayDeleting), true, "deletion")
}

func TestHTTPRouteReconcilePredicate(t *testing.T) {
	cfg := testConfig()
	base := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Namespace:   "default",
		Name:        "api",
		Generation:  1,
		Annotations: map[string]string{"example.com/foreign": "one"},
		Finalizers:  []string{"example.com/foreign"},
	}}
	pred := HTTPRouteReconcilePredicate(cfg)

	testPredicateUpdate(t, pred, base, mutateRoute(base, func(route *gatewayv1.HTTPRoute) {
		route.Status.Parents = []gatewayv1.RouteParentStatus{{ControllerName: cfg.ControllerName}}
	}), false, "status only")
	testPredicateUpdate(t, pred, base, mutateRoute(base, func(route *gatewayv1.HTTPRoute) {
		route.Annotations["example.com/foreign"] = "two"
	}), false, "foreign annotation")
	testPredicateUpdate(t, pred, base, mutateRoute(base, func(route *gatewayv1.HTTPRoute) {
		route.Annotations[cfg.RouteGatewayUIDAnnotation()] = "gateway-uid"
	}), true, "binding annotation")
	testPredicateUpdate(t, pred, base, mutateRoute(base, func(route *gatewayv1.HTTPRoute) {
		route.Finalizers = append(route.Finalizers, cfg.RouteFinalizer())
	}), true, "controller finalizer")
	testPredicateUpdate(t, pred, base, mutateRoute(base, func(route *gatewayv1.HTTPRoute) {
		route.Finalizers = append(route.Finalizers, cfg.RouteBindingFinalizer("cluster", "project", "default", "edge", "gateway-uid"))
	}), true, "binding finalizer")
	testPredicateUpdate(t, pred, base, mutateRoute(base, func(route *gatewayv1.HTTPRoute) {
		route.Generation++
	}), true, "generation")
	testPredicateUpdate(t, pred, base, mutateRoute(base, markRouteDeleting), true, "deletion")
}

func TestHTTPRouteForGatewayPredicate(t *testing.T) {
	cfg := testConfig()
	otherController := gatewayv1.GatewayController("example.com/other")
	base := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "api", Generation: 1}}
	pred := HTTPRouteForGatewayPredicate(cfg)

	testPredicateUpdate(t, pred, base, mutateRoute(base, func(route *gatewayv1.HTTPRoute) {
		route.Status.Parents = []gatewayv1.RouteParentStatus{{ControllerName: otherController}}
	}), false, "foreign parent status")
	testPredicateUpdate(t, pred, base, mutateRoute(base, func(route *gatewayv1.HTTPRoute) {
		route.Status.Parents = []gatewayv1.RouteParentStatus{{ControllerName: cfg.ControllerName}}
	}), true, "controller parent status")
	testPredicateUpdate(t, pred, base, mutateRoute(base, func(route *gatewayv1.HTTPRoute) {
		route.Annotations = map[string]string{cfg.RouteGatewayNameAnnotation(): "edge"}
	}), true, "stored Gateway binding")
}

func TestGatewayForHTTPRoutePredicate(t *testing.T) {
	cfg := testConfig()
	base := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", Generation: 1}}
	base.Status.Conditions = []metav1.Condition{{
		Type:               string(gatewayv1.GatewayConditionProgrammed),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 1,
		Reason:             string(gatewayv1.GatewayReasonProgrammed),
		Message:            "ready",
	}, {
		Type:               string(gatewayv1.GatewayConditionAccepted),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 1,
		Reason:             string(gatewayv1.GatewayReasonAccepted),
		Message:            "accepted",
	}}
	pred := GatewayForHTTPRoutePredicate(cfg)

	testPredicateUpdate(t, pred, base, mutateGateway(base, func(gateway *gatewayv1.Gateway) {
		gateway.Status.Addresses = []gatewayv1.GatewayStatusAddress{{Value: "192.0.2.10"}}
		gateway.Status.Listeners = []gatewayv1.ListenerStatus{{Name: "http", AttachedRoutes: 1}}
	}), false, "unconsumed Gateway status")
	testPredicateUpdate(t, pred, base, mutateGateway(base, func(gateway *gatewayv1.Gateway) {
		gateway.Status.Conditions[0].Message = "still ready"
	}), false, "Programmed message")
	testPredicateUpdate(t, pred, base, mutateGateway(base, func(gateway *gatewayv1.Gateway) {
		gateway.Status.Conditions[0].Status = metav1.ConditionFalse
	}), true, "Programmed status")
	testPredicateUpdate(t, pred, base, mutateGateway(base, func(gateway *gatewayv1.Gateway) {
		gateway.Status.Conditions[0].ObservedGeneration = 2
	}), true, "Programmed observed generation")
	testPredicateUpdate(t, pred, base, mutateGateway(base, func(gateway *gatewayv1.Gateway) {
		gateway.Status.Conditions[0].Reason = "DifferentReason"
	}), true, "Programmed reason")
	testPredicateUpdate(t, pred, base, mutateGateway(base, func(gateway *gatewayv1.Gateway) {
		gateway.Status.Conditions[1].Message = "still accepted"
	}), false, "Accepted message")
	testPredicateUpdate(t, pred, base, mutateGateway(base, func(gateway *gatewayv1.Gateway) {
		gateway.Status.Conditions[1].Status = metav1.ConditionFalse
	}), true, "Accepted status")
	testPredicateUpdate(t, pred, base, mutateGateway(base, func(gateway *gatewayv1.Gateway) {
		gateway.Status.Conditions[1].Reason = "DifferentReason"
	}), true, "Accepted reason")
	testPredicateUpdate(t, pred, base, mutateGateway(base, func(gateway *gatewayv1.Gateway) {
		gateway.Annotations = map[string]string{cfg.GatewayListenerPortAnnotation(): "80"}
	}), true, "Gateway binding")
}

func TestGatewayClassAndPeerPredicates(t *testing.T) {
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 1}}
	classPredicate := GatewayClassReconcilePredicate()
	testPredicateUpdate(t, classPredicate, class, class.DeepCopy(), false, "unchanged GatewayClass")
	foreignStatus := class.DeepCopy()
	foreignStatus.Status.Conditions = []metav1.Condition{{Type: "example.net/Foreign"}}
	testPredicateUpdate(t, classPredicate, class, foreignStatus, false, "foreign GatewayClass status")
	changedStatus := class.DeepCopy()
	changedStatus.Status.Conditions = []metav1.Condition{{Type: string(gatewayv1.GatewayClassConditionStatusAccepted)}}
	testPredicateUpdate(t, classPredicate, class, changedStatus, true, "owned GatewayClass status")
	changedFinalizer := class.DeepCopy()
	changedFinalizer.Finalizers = []string{gatewayv1.GatewayClassFinalizerGatewaysExist}
	testPredicateUpdate(t, classPredicate, class, changedFinalizer, true, "GatewayClass finalizer")
	changedClassGeneration := class.DeepCopy()
	changedClassGeneration.Generation++
	testPredicateUpdate(t, classPredicate, class, changedClassGeneration, true, "GatewayClass generation")
	deletingClass := class.DeepCopy()
	markDeleting(&deletingClass.ObjectMeta)
	testPredicateUpdate(t, classPredicate, class, deletingClass, true, "GatewayClass deletion")

	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "api", Generation: 1}}
	peerPredicate := HTTPRoutePeerPredicate()
	statusChanged := route.DeepCopy()
	statusChanged.Status.Parents = []gatewayv1.RouteParentStatus{{}}
	testPredicateUpdate(t, peerPredicate, route, statusChanged, false, "peer status")
	bindingChanged := route.DeepCopy()
	bindingChanged.Annotations = map[string]string{"example.com/binding": "value"}
	testPredicateUpdate(t, peerPredicate, route, bindingChanged, false, "peer metadata")
	specChanged := route.DeepCopy()
	specChanged.Generation++
	testPredicateUpdate(t, peerPredicate, route, specChanged, true, "peer generation")
	deletingRoute := route.DeepCopy()
	markDeleting(&deletingRoute.ObjectMeta)
	testPredicateUpdate(t, peerPredicate, route, deletingRoute, true, "peer deletion")
}

func TestGatewayClassForGatewayPredicate(t *testing.T) {
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 1}}
	SetCondition(&class.Status.Conditions, Condition(
		string(gatewayv1.GatewayClassConditionStatusAccepted),
		metav1.ConditionTrue,
		string(gatewayv1.GatewayClassReasonAccepted),
		"GatewayClass is accepted",
		class.Generation,
	))
	SetCondition(&class.Status.Conditions, Condition(
		string(gatewayv1.GatewayClassConditionStatusSupportedVersion),
		metav1.ConditionTrue,
		string(gatewayv1.GatewayClassReasonSupportedVersion),
		"Gateway API version is supported",
		class.Generation,
	))
	pred := GatewayClassForGatewayPredicate()
	testPredicateUpdate(t, pred, class, class.DeepCopy(), false, "unchanged GatewayClass")
	messageChanged := class.DeepCopy()
	meta.FindStatusCondition(messageChanged.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion)).Message = "still supported"
	testPredicateUpdate(t, pred, class, messageChanged, false, "SupportedVersion message")
	unsupported := class.DeepCopy()
	condition := meta.FindStatusCondition(unsupported.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion))
	condition.Status = metav1.ConditionFalse
	condition.Reason = string(gatewayv1.GatewayClassReasonUnsupportedVersion)
	testPredicateUpdate(t, pred, class, unsupported, true, "SupportedVersion status")
	acceptedReasonChanged := class.DeepCopy()
	meta.FindStatusCondition(acceptedReasonChanged.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted)).Reason = "DifferentReason"
	testPredicateUpdate(t, pred, class, acceptedReasonChanged, true, "Accepted reason")
}

func TestGatewayAPICRDReconcilePredicate(t *testing.T) {
	pred := GatewayAPICRDReconcilePredicate()
	definition := gatewayAPICRD("gateways.gateway.networking.k8s.io", gatewayconsts.BundleVersion)
	if !pred.Create(event.CreateEvent{Object: definition}) {
		t.Fatal("Gateway API CRD create was ignored")
	}
	if !pred.Delete(event.DeleteEvent{Object: definition}) {
		t.Fatal("Gateway API CRD delete was ignored")
	}
	statusChanged := definition.DeepCopy()
	statusChanged.Status.Conditions = append(statusChanged.Status.Conditions, apiextensionsv1.CustomResourceDefinitionCondition{
		Type: apiextensionsv1.Established,
	})
	testPredicateUpdate(t, pred, definition, statusChanged, false, "CRD status")
	annotationChanged := definition.DeepCopy()
	annotationChanged.Annotations[gatewayconsts.BundleVersionAnnotation] = "v1.5.0"
	testPredicateUpdate(t, pred, definition, annotationChanged, true, "bundle version annotation")
	specChanged := definition.DeepCopy()
	specChanged.Generation++
	testPredicateUpdate(t, pred, definition, specChanged, true, "CRD generation")
	otherGroup := gatewayAPICRD("widgets.example.com", gatewayconsts.BundleVersion)
	otherGroup.Spec.Group = "example.com"
	if pred.Create(event.CreateEvent{Object: otherGroup}) {
		t.Fatal("CRD from another API group triggered reconciliation")
	}
	otherGroupChanged := otherGroup.DeepCopy()
	otherGroupChanged.Annotations[gatewayconsts.BundleVersionAnnotation] = "v2.0.0"
	testPredicateUpdate(t, pred, otherGroup, otherGroupChanged, false, "another CRD group")
	nestedGroup := gatewayAPICRD("widgets.foo.gateway.networking.k8s.io", gatewayconsts.BundleVersion)
	nestedGroup.Spec.Group = "foo.gateway.networking.k8s.io"
	if pred.Create(event.CreateEvent{Object: nestedGroup}) {
		t.Fatal("CRD whose group only ends with the Gateway API group triggered reconciliation")
	}
	metadata := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
		Name:        definition.Name,
		Generation:  definition.Generation,
		Annotations: map[string]string{gatewayconsts.BundleVersionAnnotation: gatewayconsts.BundleVersion},
	}}
	metadataChanged := metadata.DeepCopy()
	metadataChanged.Annotations[gatewayconsts.BundleVersionAnnotation] = "v1.5.0"
	if !pred.Create(event.CreateEvent{Object: metadata}) {
		t.Fatal("metadata-only Gateway API CRD create was ignored")
	}
	testPredicateUpdate(t, pred, metadata, metadataChanged, true, "metadata-only bundle version annotation")
}

func TestServicePredicate(t *testing.T) {
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "backend", Generation: 1}}
	pred := ServicePredicate()
	statusChanged := service.DeepCopy()
	statusChanged.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.10"}}
	testPredicateUpdate(t, pred, service, statusChanged, false, "Service status")
	specChanged := service.DeepCopy()
	specChanged.Generation++
	testPredicateUpdate(t, pred, service, specChanged, true, "Service generation")
}

func TestEndpointSlicePredicate(t *testing.T) {
	ready := true
	workerOne := "worker-1"
	base := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "backend-1",
			Labels:    map[string]string{discoveryv1.LabelServiceName: "backend"},
		},
		Endpoints: []discoveryv1.Endpoint{{NodeName: &workerOne, Conditions: discoveryv1.EndpointConditions{Ready: &ready}}},
	}
	pred := EndpointSlicePredicate()

	irrelevant := base.DeepCopy()
	irrelevant.Endpoints[0].Addresses = []string{"10.0.0.10"}
	testPredicateUpdate(t, pred, base, irrelevant, false, "endpoint address")

	notReady := false
	readinessChanged := base.DeepCopy()
	readinessChanged.Endpoints[0].Conditions.Ready = &notReady
	testPredicateUpdate(t, pred, base, readinessChanged, true, "endpoint readiness")

	workerTwo := "worker-2"
	nodeChanged := base.DeepCopy()
	nodeChanged.Endpoints[0].NodeName = &workerTwo
	testPredicateUpdate(t, pred, base, nodeChanged, true, "endpoint Node")

	serviceChanged := base.DeepCopy()
	serviceChanged.Labels[discoveryv1.LabelServiceName] = "other-backend"
	testPredicateUpdate(t, pred, base, serviceChanged, true, "Service label")

	deleting := base.DeepCopy()
	markDeleting(&deleting.ObjectMeta)
	testPredicateUpdate(t, pred, base, deleting, true, "EndpointSlice deletion")
}

func TestNodePredicate(t *testing.T) {
	base := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.10"},
				{Type: corev1.NodeExternalIP, Address: "192.0.2.10"},
			},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	pred := NodePredicate(corev1.NodeInternalIP)

	labelChanged := base.DeepCopy()
	labelChanged.Labels = map[string]string{"example.com/zone": "one"}
	testPredicateUpdate(t, pred, base, labelChanged, false, "Node label")

	externalAddressChanged := base.DeepCopy()
	externalAddressChanged.Status.Addresses[1].Address = "192.0.2.11"
	testPredicateUpdate(t, pred, base, externalAddressChanged, false, "unselected address")

	internalAddressChanged := base.DeepCopy()
	internalAddressChanged.Status.Addresses[0].Address = "10.0.0.11"
	testPredicateUpdate(t, pred, base, internalAddressChanged, true, "selected address")

	messageChanged := base.DeepCopy()
	messageChanged.Status.Conditions[0].Message = "heartbeat"
	testPredicateUpdate(t, pred, base, messageChanged, false, "readiness message")

	readinessChanged := base.DeepCopy()
	readinessChanged.Status.Conditions[0].Status = corev1.ConditionFalse
	testPredicateUpdate(t, pred, base, readinessChanged, true, "readiness")

	unschedulable := base.DeepCopy()
	unschedulable.Spec.Unschedulable = true
	testPredicateUpdate(t, pred, base, unschedulable, true, "schedulability")

	deleting := base.DeepCopy()
	markDeleting(&deleting.ObjectMeta)
	testPredicateUpdate(t, pred, base, deleting, true, "Node deletion")
}

func TestPredicatesAcceptLifecycleEvents(t *testing.T) {
	object := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge"}}
	pred := GatewayReconcilePredicate(testConfig())
	if !pred.Create(event.CreateEvent{Object: object}) {
		t.Fatal("create event was filtered")
	}
	if !pred.Delete(event.DeleteEvent{Object: object}) {
		t.Fatal("delete event was filtered")
	}
	if !pred.Generic(event.GenericEvent{Object: object}) {
		t.Fatal("generic event was filtered")
	}
	if !pred.Update(event.UpdateEvent{ObjectNew: object}) {
		t.Fatal("incomplete update event was filtered")
	}
}

func testPredicateUpdate(
	t *testing.T,
	pred predicate.Predicate,
	oldObject client.Object,
	newObject client.Object,
	want bool,
	name string,
) {
	t.Helper()
	if got := pred.Update(event.UpdateEvent{ObjectOld: oldObject, ObjectNew: newObject}); got != want {
		t.Errorf("%s update accepted = %t, want %t", name, got, want)
	}
}

func mutateGateway(base *gatewayv1.Gateway, mutate func(*gatewayv1.Gateway)) *gatewayv1.Gateway {
	copy := base.DeepCopy()
	mutate(copy)
	return copy
}

func mutateRoute(base *gatewayv1.HTTPRoute, mutate func(*gatewayv1.HTTPRoute)) *gatewayv1.HTTPRoute {
	copy := base.DeepCopy()
	mutate(copy)
	return copy
}

func markGatewayDeleting(gateway *gatewayv1.Gateway) {
	markDeleting(&gateway.ObjectMeta)
}

func markRouteDeleting(route *gatewayv1.HTTPRoute) {
	markDeleting(&route.ObjectMeta)
}

func markDeleting(metadata *metav1.ObjectMeta) {
	timestamp := metav1.NewTime(time.Unix(1, 0))
	metadata.DeletionTimestamp = &timestamp
}
