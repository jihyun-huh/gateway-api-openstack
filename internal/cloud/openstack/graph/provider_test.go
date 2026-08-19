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

package graph

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

func TestProviderIdentityRequiresAuthenticatedProject(t *testing.T) {
	value := cloud.Identity{
		OpenStackProjectID: "project-a",
		ClusterID:          "cluster-a",
		Controller:         "example.com/openstack",
		GatewayNamespace:   "default",
		GatewayName:        "edge",
		GatewayUID:         "gateway-uid",
	}
	provider := NewProvider(ServiceClients{ProjectID: "project-a"}, ProviderConfig{})
	if _, err := provider.identity(value); err != nil {
		t.Fatalf("identity() error = %v", err)
	}
	value.OpenStackProjectID = "project-b"
	if _, err := provider.identity(value); err == nil {
		t.Fatal("identity() accepted a different project")
	}
	value.OpenStackProjectID = ""
	if _, err := provider.identity(value); err == nil {
		t.Fatal("identity() accepted an empty project")
	}
}

func TestEnsureGatewayClassifiesGophercloudFailure(t *testing.T) {
	identity := safetyTestIdentity(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
	})
	provider := NewProvider(ServiceClients{
		LoadBalancer: safetyServiceClient(handler),
		ProjectID:    "project-a",
	}, ProviderConfig{})

	_, err := provider.EnsureGateway(context.Background(), gatewayTransitionSpec(identity))
	if !errors.Is(err, cloud.ErrRateLimited) {
		t.Fatalf("EnsureGateway() error = %v, want rate limit category", err)
	}
	var response gophercloud.ErrUnexpectedResponseCode
	if !errors.As(err, &response) || response.Actual != http.StatusTooManyRequests {
		t.Fatalf("EnsureGateway() error did not retain Gophercloud response: %v", err)
	}
}
