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
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayconsts "sigs.k8s.io/gateway-api/pkg/consts"
)

func updatePredicate(changed func(client.Object, client.Object) bool) predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(event event.UpdateEvent) bool {
			if event.ObjectOld == nil || event.ObjectNew == nil {
				return true
			}
			return changed(event.ObjectOld, event.ObjectNew)
		},
	}
}

func generationOrDeletionChanged(oldObject, newObject client.Object) bool {
	return oldObject.GetGeneration() != newObject.GetGeneration() ||
		!equality.Semantic.DeepEqual(oldObject.GetDeletionTimestamp(), newObject.GetDeletionTimestamp())
}

func gatewayClassReconcilePredicate() predicate.Predicate {
	return updatePredicate(func(oldObject, newObject client.Object) bool {
		oldClass, oldOK := oldObject.(*gatewayv1.GatewayClass)
		newClass, newOK := newObject.(*gatewayv1.GatewayClass)
		if !oldOK || !newOK {
			return true
		}
		return generationOrDeletionChanged(oldClass, newClass) ||
			finalizerPresenceChanged(oldClass, newClass, gatewayv1.GatewayClassFinalizerGatewaysExist) ||
			gatewayClassConditionStateFor(oldClass, gatewayv1.GatewayClassConditionStatusAccepted) !=
				gatewayClassConditionStateFor(newClass, gatewayv1.GatewayClassConditionStatusAccepted) ||
			gatewayClassConditionStateFor(oldClass, gatewayv1.GatewayClassConditionStatusSupportedVersion) !=
				gatewayClassConditionStateFor(newClass, gatewayv1.GatewayClassConditionStatusSupportedVersion)
	})
}

func gatewayReconcilePredicate(config Config) predicate.Predicate {
	return updatePredicate(func(oldObject, newObject client.Object) bool {
		oldGateway, oldOK := oldObject.(*gatewayv1.Gateway)
		newGateway, newOK := newObject.(*gatewayv1.Gateway)
		if !oldOK || !newOK {
			return true
		}
		return generationOrDeletionChanged(oldGateway, newGateway) ||
			gatewayBindingChanged(config, oldGateway, newGateway)
	})
}

func gatewayClassForGatewayPredicate() predicate.Predicate {
	return updatePredicate(func(oldObject, newObject client.Object) bool {
		oldClass, oldOK := oldObject.(*gatewayv1.GatewayClass)
		newClass, newOK := newObject.(*gatewayv1.GatewayClass)
		if !oldOK || !newOK {
			return true
		}
		return generationOrDeletionChanged(oldClass, newClass) ||
			gatewayClassConditionStateFor(oldClass, gatewayv1.GatewayClassConditionStatusAccepted) !=
				gatewayClassConditionStateFor(newClass, gatewayv1.GatewayClassConditionStatusAccepted) ||
			gatewayClassConditionStateFor(oldClass, gatewayv1.GatewayClassConditionStatusSupportedVersion) !=
				gatewayClassConditionStateFor(newClass, gatewayv1.GatewayClassConditionStatusSupportedVersion)
	})
}

func gatewayAPICRDReconcilePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event event.CreateEvent) bool {
			return isGatewayAPICRD(event.Object)
		},
		DeleteFunc: func(event event.DeleteEvent) bool {
			return isGatewayAPICRD(event.Object)
		},
		UpdateFunc: func(event event.UpdateEvent) bool {
			if event.ObjectOld == nil || event.ObjectNew == nil {
				return true
			}
			if !isGatewayAPICRDName(event.ObjectOld.GetName()) && !isGatewayAPICRDName(event.ObjectNew.GetName()) {
				return false
			}
			return generationOrDeletionChanged(event.ObjectOld, event.ObjectNew) ||
				event.ObjectOld.GetName() != event.ObjectNew.GetName() ||
				event.ObjectOld.GetAnnotations()[gatewayconsts.BundleVersionAnnotation] !=
					event.ObjectNew.GetAnnotations()[gatewayconsts.BundleVersionAnnotation]
		},
		GenericFunc: func(event event.GenericEvent) bool {
			return isGatewayAPICRD(event.Object)
		},
	}
}

func isGatewayAPICRD(object client.Object) bool {
	return object != nil && isGatewayAPICRDName(object.GetName())
}

func httpRouteReconcilePredicate(config Config) predicate.Predicate {
	return updatePredicate(func(oldObject, newObject client.Object) bool {
		oldRoute, oldOK := oldObject.(*gatewayv1.HTTPRoute)
		newRoute, newOK := newObject.(*gatewayv1.HTTPRoute)
		if !oldOK || !newOK {
			return true
		}
		return generationOrDeletionChanged(oldRoute, newRoute) ||
			routeBindingChanged(config, oldRoute, newRoute)
	})
}

func httpRoutePeerPredicate() predicate.Predicate {
	return updatePredicate(generationOrDeletionChanged)
}

func httpRouteForGatewayPredicate(config Config) predicate.Predicate {
	return updatePredicate(func(oldObject, newObject client.Object) bool {
		oldRoute, oldOK := oldObject.(*gatewayv1.HTTPRoute)
		newRoute, newOK := newObject.(*gatewayv1.HTTPRoute)
		if !oldOK || !newOK {
			return true
		}
		return generationOrDeletionChanged(oldRoute, newRoute) ||
			routeBindingChanged(config, oldRoute, newRoute) ||
			!equality.Semantic.DeepEqual(
				controllerRouteParentStatuses(oldRoute, config.ControllerName),
				controllerRouteParentStatuses(newRoute, config.ControllerName),
			)
	})
}

func gatewayForHTTPRoutePredicate(config Config) predicate.Predicate {
	return updatePredicate(func(oldObject, newObject client.Object) bool {
		oldGateway, oldOK := oldObject.(*gatewayv1.Gateway)
		newGateway, newOK := newObject.(*gatewayv1.Gateway)
		if !oldOK || !newOK {
			return true
		}
		return generationOrDeletionChanged(oldGateway, newGateway) ||
			gatewayBindingChanged(config, oldGateway, newGateway) ||
			gatewayAcceptedState(oldGateway) != gatewayAcceptedState(newGateway) ||
			gatewayProgrammedState(oldGateway) != gatewayProgrammedState(newGateway)
	})
}

func endpointSlicePredicate() predicate.Predicate {
	return updatePredicate(func(oldObject, newObject client.Object) bool {
		oldSlice, oldOK := oldObject.(*discoveryv1.EndpointSlice)
		newSlice, newOK := newObject.(*discoveryv1.EndpointSlice)
		if !oldOK || !newOK {
			return true
		}
		return endpointSliceBackendState(oldSlice) != endpointSliceBackendState(newSlice)
	})
}

func nodePredicate(addressType corev1.NodeAddressType) predicate.Predicate {
	return updatePredicate(func(oldObject, newObject client.Object) bool {
		oldNode, oldOK := oldObject.(*corev1.Node)
		newNode, newOK := newObject.(*corev1.Node)
		if !oldOK || !newOK {
			return true
		}
		return nodeBackendStateFor(oldNode, addressType) != nodeBackendStateFor(newNode, addressType)
	})
}

func servicePredicate() predicate.Predicate {
	return predicate.GenerationChangedPredicate{}
}

func finalizerPresenceChanged(oldObject, newObject client.Object, finalizer string) bool {
	return containsString(oldObject.GetFinalizers(), finalizer) != containsString(newObject.GetFinalizers(), finalizer)
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func gatewayBindingChanged(config Config, oldGateway, newGateway *gatewayv1.Gateway) bool {
	if finalizerPresenceChanged(oldGateway, newGateway, config.gatewayFinalizer()) {
		return true
	}
	for _, key := range []string{
		config.gatewayListenerPortAnnotation(),
		config.gatewayClusterIDAnnotation(),
		config.gatewayProjectIDAnnotation(),
	} {
		if oldGateway.Annotations[key] != newGateway.Annotations[key] {
			return true
		}
	}
	return false
}

func routeBindingChanged(config Config, oldRoute, newRoute *gatewayv1.HTTPRoute) bool {
	for _, key := range []string{
		config.routeGatewayNamespaceAnnotation(),
		config.routeGatewayNameAnnotation(),
		config.routeGatewayUIDAnnotation(),
		config.routeClusterIDAnnotation(),
		config.routeProjectIDAnnotation(),
	} {
		if oldRoute.Annotations[key] != newRoute.Annotations[key] {
			return true
		}
	}
	return !equality.Semantic.DeepEqual(
		controllerRouteFinalizers(config, oldRoute),
		controllerRouteFinalizers(config, newRoute),
	)
}

func controllerRouteFinalizers(config Config, route *gatewayv1.HTTPRoute) []string {
	finalizers := make([]string, 0, 2)
	for _, finalizer := range route.Finalizers {
		if finalizer == config.routeFinalizer() || strings.HasPrefix(finalizer, config.routeBindingFinalizerPrefix()) {
			finalizers = append(finalizers, finalizer)
		}
	}
	sort.Strings(finalizers)
	return finalizers
}

func controllerRouteParentStatuses(route *gatewayv1.HTTPRoute, controllerName gatewayv1.GatewayController) []gatewayv1.RouteParentStatus {
	statuses := make([]gatewayv1.RouteParentStatus, 0, len(route.Status.Parents))
	for _, status := range route.Status.Parents {
		if status.ControllerName == controllerName {
			statuses = append(statuses, status)
		}
	}
	return statuses
}

type programmedState struct {
	present            bool
	status             string
	reason             string
	observedGeneration int64
}

type gatewayClassConditionState struct {
	present            bool
	status             metav1.ConditionStatus
	reason             string
	observedGeneration int64
}

func gatewayClassConditionStateFor(
	gatewayClass *gatewayv1.GatewayClass,
	conditionType gatewayv1.GatewayClassConditionType,
) gatewayClassConditionState {
	condition := meta.FindStatusCondition(gatewayClass.Status.Conditions, string(conditionType))
	if condition == nil {
		return gatewayClassConditionState{}
	}
	return gatewayClassConditionState{
		present:            true,
		status:             condition.Status,
		reason:             condition.Reason,
		observedGeneration: condition.ObservedGeneration,
	}
}

func gatewayAcceptedState(gateway *gatewayv1.Gateway) programmedState {
	condition := meta.FindStatusCondition(gateway.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	if condition == nil {
		return programmedState{}
	}
	return programmedState{
		present:            true,
		status:             string(condition.Status),
		reason:             condition.Reason,
		observedGeneration: condition.ObservedGeneration,
	}
}

func gatewayProgrammedState(gateway *gatewayv1.Gateway) programmedState {
	condition := meta.FindStatusCondition(gateway.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	if condition == nil {
		return programmedState{}
	}
	return programmedState{
		present:            true,
		status:             string(condition.Status),
		reason:             condition.Reason,
		observedGeneration: condition.ObservedGeneration,
	}
}

type endpointBackendState struct {
	serviceName string
	hasReady    bool
	readyNodes  string
	deleting    bool
}

func endpointSliceBackendState(endpointSlice *discoveryv1.EndpointSlice) endpointBackendState {
	nodes := make(map[string]struct{})
	hasReady := false
	for _, endpoint := range endpointSlice.Endpoints {
		if !endpointReady(endpoint.Conditions) {
			continue
		}
		hasReady = true
		if endpoint.NodeName != nil {
			nodes[*endpoint.NodeName] = struct{}{}
		}
	}
	readyNodes := make([]string, 0, len(nodes))
	for node := range nodes {
		readyNodes = append(readyNodes, node)
	}
	sort.Strings(readyNodes)
	return endpointBackendState{
		serviceName: endpointSlice.Labels[discoveryv1.LabelServiceName],
		hasReady:    hasReady,
		readyNodes:  strings.Join(readyNodes, "\x00"),
		deleting:    !endpointSlice.DeletionTimestamp.IsZero(),
	}
}

type nodeBackendState struct {
	address       string
	ready         corev1.ConditionStatus
	readyPresent  bool
	unschedulable bool
	deleting      bool
}

func nodeBackendStateFor(node *corev1.Node, addressType corev1.NodeAddressType) nodeBackendState {
	state := nodeBackendState{
		address:       nodeAddress(node, addressType),
		unschedulable: node.Spec.Unschedulable,
		deleting:      !node.DeletionTimestamp.IsZero(),
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			state.ready = condition.Status
			state.readyPresent = true
			break
		}
	}
	return state
}
