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
	"testing"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

type recordedFieldIndex struct {
	object  client.Object
	extract client.IndexerFunc
}

type recordingFieldIndexer struct {
	indexes   map[string]recordedFieldIndex
	failField string
	failErr   error
}

func (r *recordingFieldIndexer) IndexField(
	_ context.Context,
	object client.Object,
	field string,
	extract client.IndexerFunc,
) error {
	if field == r.failField {
		return r.failErr
	}
	if r.indexes == nil {
		r.indexes = make(map[string]recordedFieldIndex)
	}
	r.indexes[field] = recordedFieldIndex{object: object, extract: extract}
	return nil
}

func TestSetupIndexes(t *testing.T) {
	t.Parallel()

	config := testConfig()
	indexer := &recordingFieldIndexer{}
	if err := SetupIndexes(context.Background(), indexer, config); err != nil {
		t.Fatalf("SetupIndexes() error = %v", err)
	}

	wantTypes := map[string]client.Object{
		IndexGatewayClassByController:   &gatewayv1.GatewayClass{},
		IndexGatewayByClass:             &gatewayv1.Gateway{},
		IndexHTTPRouteByParentGateway:   &gatewayv1.HTTPRoute{},
		IndexHTTPRouteByStatusGateway:   &gatewayv1.HTTPRoute{},
		IndexHTTPRouteByBackendService:  &gatewayv1.HTTPRoute{},
		IndexHTTPRouteByBoundGateway:    &gatewayv1.HTTPRoute{},
		indexHTTPRouteByBoundGatewayUID: &gatewayv1.HTTPRoute{},
		IndexHTTPRouteByNodeBackend:     &gatewayv1.HTTPRoute{},
		IndexEndpointSliceByService:     &discoveryv1.EndpointSlice{},
	}
	if len(indexer.indexes) != len(wantTypes) {
		t.Fatalf("registered indexes = %d, want %d", len(indexer.indexes), len(wantTypes))
	}
	for field, wantObject := range wantTypes {
		index, found := indexer.indexes[field]
		if !found {
			t.Errorf("field index %q was not registered", field)
			continue
		}
		if reflect.TypeOf(index.object) != reflect.TypeOf(wantObject) {
			t.Errorf("field index %q object type = %T, want %T", field, index.object, wantObject)
		}
	}

	gatewayClass := &gatewayv1.GatewayClass{Spec: gatewayv1.GatewayClassSpec{ControllerName: config.ControllerName}}
	assertIndexValues(t, indexer, IndexGatewayClassByController, gatewayClass, []string{string(config.ControllerName)})

	gateway := &gatewayv1.Gateway{Spec: gatewayv1.GatewaySpec{GatewayClassName: "openstack"}}
	assertIndexValues(t, indexer, IndexGatewayByClass, gateway, []string{"openstack"})

	route := indexedTestHTTPRoute(config)
	assertIndexValues(t, indexer, IndexHTTPRouteByParentGateway, route, []string{"gateways/edge", "routes/z-gateway"})
	assertIndexValues(t, indexer, IndexHTTPRouteByStatusGateway, route, []string{"gateways/edge", "routes/status-gateway"})
	assertIndexValues(t, indexer, IndexHTTPRouteByBackendService, route, []string{"apps/a-service", "routes/z-service"})
	assertIndexValues(t, indexer, IndexHTTPRouteByBoundGateway, route, []string{"gateways/edge"})
	assertIndexValues(t, indexer, indexHTTPRouteByBoundGatewayUID, route, []string{"gateway-uid"})
	assertIndexValues(t, indexer, IndexHTTPRouteByNodeBackend, route, []string{NodeBackendIndexValue})

	endpointSlice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
		Namespace: "apps",
		Labels:    map[string]string{discoveryv1.LabelServiceName: "a-service"},
	}}
	assertIndexValues(t, indexer, IndexEndpointSliceByService, endpointSlice, []string{"apps/a-service"})
}

func TestSetupIndexesReturnsRegistrationError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("index unavailable")
	indexer := &recordingFieldIndexer{
		failField: IndexHTTPRouteByBackendService,
		failErr:   wantErr,
	}
	err := SetupIndexes(context.Background(), indexer, testConfig())
	if !errors.Is(err, wantErr) {
		t.Fatalf("SetupIndexes() error = %v, want error wrapping %v", err, wantErr)
	}
}

func TestParentGatewayKeys(t *testing.T) {
	t.Parallel()

	gatewayGroup := gatewayv1.Group(gatewayv1.GroupName)
	gatewayKind := gatewayv1.Kind("Gateway")
	coreGroup := gatewayv1.Group("")
	serviceKind := gatewayv1.Kind("Service")
	otherNamespace := gatewayv1.Namespace("gateways")
	localNamespace := gatewayv1.Namespace("routes")

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "routes"},
		Spec: gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{
			{Name: "z-gateway"},
			{Name: "z-gateway", Namespace: &localNamespace},
			{Group: &gatewayGroup, Kind: &gatewayKind, Namespace: &otherNamespace, Name: "edge"},
			{Group: &coreGroup, Kind: &serviceKind, Name: "ignored"},
		}}},
	}

	want := []string{"gateways/edge", "routes/z-gateway"}
	if got := ParentGatewayKeys(route); !reflect.DeepEqual(got, want) {
		t.Fatalf("ParentGatewayKeys() = %#v, want %#v", got, want)
	}
}

func TestBackendServiceKeys(t *testing.T) {
	t.Parallel()

	coreGroup := gatewayv1.Group("")
	serviceKind := gatewayv1.Kind("Service")
	configMapKind := gatewayv1.Kind("ConfigMap")
	externalGroup := gatewayv1.Group("example.com")
	appsNamespace := gatewayv1.Namespace("apps")
	routesNamespace := gatewayv1.Namespace("routes")

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "routes"},
		Spec: gatewayv1.HTTPRouteSpec{Rules: []gatewayv1.HTTPRouteRule{
			{BackendRefs: []gatewayv1.HTTPBackendRef{
				{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "z-service"}}},
				{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
					Group: &coreGroup, Kind: &serviceKind, Namespace: &appsNamespace, Name: "a-service",
				}}},
				{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
					Kind: &configMapKind, Name: "ignored-kind",
				}}},
			}},
			{BackendRefs: []gatewayv1.HTTPBackendRef{
				{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
					Namespace: &routesNamespace, Name: "z-service",
				}}},
				{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
					Group: &externalGroup, Name: "ignored-group",
				}}},
			}},
		}},
	}

	want := []string{"apps/a-service", "routes/z-service"}
	if got := backendServiceKeys(route); !reflect.DeepEqual(got, want) {
		t.Fatalf("backendServiceKeys() = %#v, want %#v", got, want)
	}
}

func TestBoundGatewayKeyRequiresCompleteStoredIdentity(t *testing.T) {
	t.Parallel()

	config := testConfig()
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		config.RouteGatewayNamespaceAnnotation(): "gateways",
		config.RouteGatewayNameAnnotation():      "edge",
		config.RouteGatewayUIDAnnotation():       "gateway-uid",
	}}}
	if got, want := boundGatewayKey(config, route), "gateways/edge"; got != want {
		t.Fatalf("boundGatewayKey() = %q, want %q", got, want)
	}

	delete(route.Annotations, config.RouteGatewayUIDAnnotation())
	if got := boundGatewayKey(config, route); got != "" {
		t.Fatalf("boundGatewayKey() with incomplete identity = %q, want empty", got)
	}
}

func TestIndexesOmitObjectsWithoutUsableKeys(t *testing.T) {
	t.Parallel()

	config := testConfig()
	indexer := &recordingFieldIndexer{}
	if err := SetupIndexes(context.Background(), indexer, config); err != nil {
		t.Fatalf("SetupIndexes() error = %v", err)
	}

	assertIndexValues(t, indexer, IndexGatewayByClass, &gatewayv1.Gateway{}, nil)
	assertIndexValues(t, indexer, IndexHTTPRouteByParentGateway, &gatewayv1.HTTPRoute{}, nil)
	assertIndexValues(t, indexer, IndexHTTPRouteByStatusGateway, &gatewayv1.HTTPRoute{}, nil)
	assertIndexValues(t, indexer, IndexHTTPRouteByBackendService, &gatewayv1.HTTPRoute{}, nil)
	assertIndexValues(t, indexer, IndexHTTPRouteByBoundGateway, &gatewayv1.HTTPRoute{}, nil)
	assertIndexValues(t, indexer, indexHTTPRouteByBoundGatewayUID, &gatewayv1.HTTPRoute{}, nil)
	assertIndexValues(t, indexer, IndexHTTPRouteByNodeBackend, &gatewayv1.HTTPRoute{}, nil)
	assertIndexValues(t, indexer, IndexEndpointSliceByService, &discoveryv1.EndpointSlice{}, nil)

	externalGroup := gatewayv1.Group("example.com")
	externalBackendRoute := &gatewayv1.HTTPRoute{Spec: gatewayv1.HTTPRouteSpec{
		Rules: []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{
			BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
				Group: &externalGroup,
				Name:  "external",
			}},
		}}}},
	}}
	assertIndexValues(t, indexer, IndexHTTPRouteByNodeBackend, externalBackendRoute, nil)
}

func indexedTestHTTPRoute(config Config) *gatewayv1.HTTPRoute {
	gatewayGroup := gatewayv1.Group(gatewayv1.GroupName)
	gatewayKind := gatewayv1.Kind("Gateway")
	otherNamespace := gatewayv1.Namespace("gateways")
	localNamespace := gatewayv1.Namespace("routes")
	appsNamespace := gatewayv1.Namespace("apps")

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "routes",
			Annotations: map[string]string{
				config.RouteGatewayNamespaceAnnotation(): "gateways",
				config.RouteGatewayNameAnnotation():      "edge",
				config.RouteGatewayUIDAnnotation():       "gateway-uid",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{
				{Name: "z-gateway"},
				{Name: "z-gateway", Namespace: &localNamespace},
				{Group: &gatewayGroup, Kind: &gatewayKind, Namespace: &otherNamespace, Name: "edge"},
			}},
			Rules: []gatewayv1.HTTPRouteRule{
				{BackendRefs: []gatewayv1.HTTPBackendRef{
					{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "z-service"}}},
					{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
						Namespace: &appsNamespace, Name: "a-service",
					}}},
				}},
				{BackendRefs: []gatewayv1.HTTPBackendRef{
					{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "z-service"}}},
				}},
			},
		},
		Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{
			{
				ParentRef: gatewayv1.ParentReference{
					Namespace: &otherNamespace,
					Name:      "edge",
				},
				ControllerName: config.ControllerName,
			},
			{
				ParentRef:      gatewayv1.ParentReference{Name: "status-gateway"},
				ControllerName: config.ControllerName,
			},
			{
				ParentRef:      gatewayv1.ParentReference{Name: "foreign"},
				ControllerName: "example.com/foreign",
			},
		}}},
	}
}

func assertIndexValues(
	t *testing.T,
	indexer *recordingFieldIndexer,
	field string,
	object client.Object,
	want []string,
) {
	t.Helper()
	index, found := indexer.indexes[field]
	if !found {
		t.Fatalf("field index %q was not registered", field)
	}
	if got := index.extract(object); !reflect.DeepEqual(got, want) {
		t.Errorf("field index %q values = %#v, want %#v", field, got, want)
	}
}
