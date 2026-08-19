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
	"fmt"
	"sort"

	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	// IndexGatewayClassByController indexes GatewayClasses by controller name.
	IndexGatewayClassByController = "spec.controllerName"
	// IndexGatewayByClass indexes Gateways by GatewayClass name.
	IndexGatewayByClass = "spec.gatewayClassName"
	// IndexHTTPRouteByParentGateway indexes HTTPRoutes by parent Gateway key.
	IndexHTTPRouteByParentGateway = "spec.parentGateway"
	// IndexHTTPRouteByStatusGateway indexes HTTPRoutes by status parent Gateway key.
	IndexHTTPRouteByStatusGateway = "status.parentGateway"
	// IndexHTTPRouteByBackendService indexes HTTPRoutes by backend Service key.
	IndexHTTPRouteByBackendService = "spec.backendService"
	// IndexHTTPRouteByBoundGateway indexes HTTPRoutes by bound Gateway key.
	IndexHTTPRouteByBoundGateway = "metadata.boundGateway"
	// indexHTTPRouteByBoundGatewayUID indexes HTTPRoutes by bound Gateway UID.
	indexHTTPRouteByBoundGatewayUID = "metadata.boundGatewayUID"
	// IndexHTTPRouteByNodeBackend indexes routes that use Node backends.
	IndexHTTPRouteByNodeBackend = "spec.nodeBackend"
	// IndexEndpointSliceByService indexes EndpointSlices by Service key.
	IndexEndpointSliceByService = "metadata.service"
	// NodeBackendIndexValue is the field value for routes that use Node backends.
	NodeBackendIndexValue = "true"
)

type controllerFieldIndex struct {
	object  client.Object
	field   string
	extract client.IndexerFunc
}

// SetupIndexes registers the field indexes shared by the Gateway API
// reconcilers and their watch mappers.
func SetupIndexes(ctx context.Context, indexer client.FieldIndexer, config Config) error {
	for _, index := range controllerFieldIndexes(config) {
		if err := indexer.IndexField(ctx, index.object, index.field, index.extract); err != nil {
			return fmt.Errorf("register %q field index: %w", index.field, err)
		}
	}
	return nil
}

func controllerFieldIndexes(config Config) []controllerFieldIndex {
	return []controllerFieldIndex{
		{
			object: &gatewayv1.GatewayClass{},
			field:  IndexGatewayClassByController,
			extract: func(object client.Object) []string {
				gatewayClass, ok := object.(*gatewayv1.GatewayClass)
				if !ok || gatewayClass.Spec.ControllerName == "" {
					return nil
				}
				return []string{string(gatewayClass.Spec.ControllerName)}
			},
		},
		{
			object: &gatewayv1.Gateway{},
			field:  IndexGatewayByClass,
			extract: func(object client.Object) []string {
				gateway, ok := object.(*gatewayv1.Gateway)
				if !ok || gateway.Spec.GatewayClassName == "" {
					return nil
				}
				return []string{string(gateway.Spec.GatewayClassName)}
			},
		},
		{
			object: &gatewayv1.HTTPRoute{},
			field:  IndexHTTPRouteByParentGateway,
			extract: func(object client.Object) []string {
				route, ok := object.(*gatewayv1.HTTPRoute)
				if !ok {
					return nil
				}
				return ParentGatewayKeys(route)
			},
		},
		{
			object: &gatewayv1.HTTPRoute{},
			field:  IndexHTTPRouteByStatusGateway,
			extract: func(object client.Object) []string {
				route, ok := object.(*gatewayv1.HTTPRoute)
				if !ok {
					return nil
				}
				return statusParentGatewayKeys(config, route)
			},
		},
		{
			object: &gatewayv1.HTTPRoute{},
			field:  IndexHTTPRouteByBackendService,
			extract: func(object client.Object) []string {
				route, ok := object.(*gatewayv1.HTTPRoute)
				if !ok {
					return nil
				}
				return backendServiceKeys(route)
			},
		},
		{
			object: &gatewayv1.HTTPRoute{},
			field:  IndexHTTPRouteByBoundGateway,
			extract: func(object client.Object) []string {
				route, ok := object.(*gatewayv1.HTTPRoute)
				if !ok {
					return nil
				}
				key := boundGatewayKey(config, route)
				if key == "" {
					return nil
				}
				return []string{key}
			},
		},
		{
			object: &gatewayv1.HTTPRoute{},
			field:  indexHTTPRouteByBoundGatewayUID,
			extract: func(object client.Object) []string {
				route, ok := object.(*gatewayv1.HTTPRoute)
				if !ok || boundGatewayKey(config, route) == "" {
					return nil
				}
				return []string{route.Annotations[config.RouteGatewayUIDAnnotation()]}
			},
		},
		{
			object: &gatewayv1.HTTPRoute{},
			field:  IndexHTTPRouteByNodeBackend,
			extract: func(object client.Object) []string {
				route, ok := object.(*gatewayv1.HTTPRoute)
				if !ok || !hasCoreServiceBackend(route) {
					return nil
				}
				return []string{NodeBackendIndexValue}
			},
		},
		{
			object: &discoveryv1.EndpointSlice{},
			field:  IndexEndpointSliceByService,
			extract: func(object client.Object) []string {
				endpointSlice, ok := object.(*discoveryv1.EndpointSlice)
				if !ok {
					return nil
				}
				serviceName := endpointSlice.Labels[discoveryv1.LabelServiceName]
				if endpointSlice.Namespace == "" || serviceName == "" {
					return nil
				}
				return []string{ObjectKeyString(types.NamespacedName{
					Namespace: endpointSlice.Namespace,
					Name:      serviceName,
				})}
			},
		},
	}
}

// ObjectKeyString returns the stable namespace and name representation used by
// field indexes.
func ObjectKeyString(key types.NamespacedName) string {
	return key.String()
}

// ParentGatewayKeys returns the distinct Gateway keys referenced by an
// HTTPRoute, in deterministic order.
func ParentGatewayKeys(route *gatewayv1.HTTPRoute) []string {
	keys := make(map[string]struct{}, len(route.Spec.ParentRefs))
	for _, parent := range route.Spec.ParentRefs {
		if !IsGatewayParentRef(parent) || parent.Name == "" {
			continue
		}
		namespace := route.Namespace
		if parent.Namespace != nil {
			namespace = string(*parent.Namespace)
		}
		if namespace == "" {
			continue
		}
		keys[ObjectKeyString(types.NamespacedName{
			Namespace: namespace,
			Name:      string(parent.Name),
		})] = struct{}{}
	}
	return sortedIndexKeys(keys)
}

func statusParentGatewayKeys(config Config, route *gatewayv1.HTTPRoute) []string {
	keys := make(map[string]struct{}, len(route.Status.Parents))
	for _, parent := range route.Status.Parents {
		if parent.ControllerName != config.ControllerName || !IsGatewayParentRef(parent.ParentRef) || parent.ParentRef.Name == "" {
			continue
		}
		namespace := route.Namespace
		if parent.ParentRef.Namespace != nil {
			namespace = string(*parent.ParentRef.Namespace)
		}
		if namespace == "" {
			continue
		}
		keys[ObjectKeyString(types.NamespacedName{
			Namespace: namespace,
			Name:      string(parent.ParentRef.Name),
		})] = struct{}{}
	}
	return sortedIndexKeys(keys)
}

func backendServiceKeys(route *gatewayv1.HTTPRoute) []string {
	keys := make(map[string]struct{})
	for _, rule := range route.Spec.Rules {
		for _, backend := range rule.BackendRefs {
			if !isCoreServiceBackend(backend.BackendObjectReference) || backend.Name == "" {
				continue
			}
			namespace := route.Namespace
			if backend.Namespace != nil {
				namespace = string(*backend.Namespace)
			}
			if namespace == "" {
				continue
			}
			keys[ObjectKeyString(types.NamespacedName{
				Namespace: namespace,
				Name:      string(backend.Name),
			})] = struct{}{}
		}
	}
	return sortedIndexKeys(keys)
}

func isCoreServiceBackend(backend gatewayv1.BackendObjectReference) bool {
	return (backend.Group == nil || *backend.Group == gatewayv1.Group("")) &&
		(backend.Kind == nil || *backend.Kind == gatewayv1.Kind("Service"))
}

func hasCoreServiceBackend(route *gatewayv1.HTTPRoute) bool {
	for _, rule := range route.Spec.Rules {
		for _, backend := range rule.BackendRefs {
			if isCoreServiceBackend(backend.BackendObjectReference) {
				return true
			}
		}
	}
	return false
}

func boundGatewayKey(config Config, route *gatewayv1.HTTPRoute) string {
	annotations := route.GetAnnotations()
	namespace := annotations[config.RouteGatewayNamespaceAnnotation()]
	name := annotations[config.RouteGatewayNameAnnotation()]
	uid := annotations[config.RouteGatewayUIDAnnotation()]
	if namespace == "" || name == "" || uid == "" {
		return ""
	}
	return ObjectKeyString(types.NamespacedName{Namespace: namespace, Name: name})
}

func sortedIndexKeys(keys map[string]struct{}) []string {
	if len(keys) == 0 {
		return nil
	}
	result := make([]string, 0, len(keys))
	for key := range keys {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}
