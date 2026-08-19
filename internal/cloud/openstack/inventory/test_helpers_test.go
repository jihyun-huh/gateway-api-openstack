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

package inventory

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
	openstackidentity "github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack/identity"
)

func safetyTestIdentity(t *testing.T) openstackidentity.Identity {
	t.Helper()
	identity, err := openstackidentity.NewIdentity(cloud.Identity{
		OpenStackProjectID: "project-a",
		ClusterID:          "test-cluster",
		Controller:         "example.com/openstack-gateway-controller",
		GatewayNamespace:   "gateway-system",
		GatewayName:        "example-gateway",
		GatewayUID:         "gateway-uid",
		RouteNamespace:     "application",
		RouteName:          "example-route",
		RouteUID:           "route-uid",
	})
	if err != nil {
		t.Fatalf("NewIdentity() error = %v", err)
	}
	return identity
}

func safetyGatewayTags(t *testing.T, identity openstackidentity.Identity, role string) []string {
	t.Helper()
	tags, err := identity.GatewayTags(role)
	if err != nil {
		t.Fatalf("GatewayTags(%q) error = %v", role, err)
	}
	return tags
}

func safetyRouteTags(t *testing.T, identity openstackidentity.Identity, role string) []string {
	t.Helper()
	tags, err := identity.RouteTags(role)
	if err != nil {
		t.Fatalf("RouteTags(%q) error = %v", role, err)
	}
	return tags
}

type safetyRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip safetyRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func safetyServiceClient(handler http.Handler) *gophercloud.ServiceClient {
	providerClient := &gophercloud.ProviderClient{TokenID: "test-token"}
	providerClient.HTTPClient.Transport = safetyRoundTripper(func(request *http.Request) (*http.Response, error) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		result := response.Result()
		result.Request = request
		return result, nil
	})
	return &gophercloud.ServiceClient{
		ProviderClient: providerClient,
		Endpoint:       "http://openstack.test/",
	}
}

func safetyWriteJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("encode test response: %v", err)
	}
}
