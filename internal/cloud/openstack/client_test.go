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

package openstack

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	tokensv3 "github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

func TestAuthenticatedProjectIDFallsBackToExplicitTenantID(t *testing.T) {
	provider := &gophercloud.ProviderClient{}
	projectID, err := authenticatedProjectID(provider, gophercloud.AuthOptions{TenantID: "project-id"})
	if err != nil {
		t.Fatalf("authenticatedProjectID() error = %v", err)
	}
	if projectID != "project-id" {
		t.Fatalf("authenticatedProjectID() = %q, want project-id", projectID)
	}
	if _, err := authenticatedProjectID(provider, gophercloud.AuthOptions{}); err == nil {
		t.Fatal("authenticatedProjectID() accepted unscoped authentication")
	}
}

func TestNewServiceClientsClassifiesInvalidConfiguration(t *testing.T) {
	_, err := NewServiceClients(context.Background(), ClientConfig{Microversion: "2.4"})
	if !errors.Is(err, cloud.ErrTerminalValidation) {
		t.Fatalf("NewServiceClients() error = %v, want terminal validation category", err)
	}
}

func TestAuthenticatedProjectIDUsesValidatedTokenResult(t *testing.T) {
	identityClient := safetyServiceClient(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/auth/tokens" {
			t.Errorf("unexpected token validation request: %s %s", request.Method, request.URL.RequestURI())
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		w.Header().Set("X-Subject-Token", "validated-token")
		safetyWriteJSON(t, w, map[string]any{"token": map[string]any{"project": map[string]string{"id": "project-from-token"}}})
	}))
	result := tokensv3.Get(context.Background(), identityClient, "input-token")
	provider := &gophercloud.ProviderClient{}
	if err := provider.SetTokenAndAuthResult(result); err != nil {
		t.Fatalf("SetTokenAndAuthResult() error = %v", err)
	}
	projectID, err := authenticatedProjectID(provider, gophercloud.AuthOptions{})
	if err != nil {
		t.Fatalf("authenticatedProjectID() error = %v", err)
	}
	if projectID != "project-from-token" {
		t.Fatalf("authenticatedProjectID() = %q, want project-from-token", projectID)
	}
}

func TestAllowReauthentication(t *testing.T) {
	projectScope := &gophercloud.AuthScope{ProjectID: "project-id"}
	tests := []struct {
		name string
		opts gophercloud.AuthOptions
		want bool
	}{
		{name: "renewable credentials", opts: gophercloud.AuthOptions{Username: "user", Password: "secret"}, want: true},
		{name: "passthrough token", opts: gophercloud.AuthOptions{TokenID: "token"}, want: false},
		{name: "passthrough token with empty scope", opts: gophercloud.AuthOptions{TokenID: "token", Scope: &gophercloud.AuthScope{}}, want: false},
		{name: "scoped token", opts: gophercloud.AuthOptions{TokenID: "token", Scope: projectScope}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := allowReauthentication(test.opts); got != test.want {
				t.Fatalf("allowReauthentication() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestValidateMicroversionUsesNumericComponents(t *testing.T) {
	for _, value := range []string{"2.5", "2.10", "3.0"} {
		if err := ValidateMicroversion(value); err != nil {
			t.Errorf("ValidateMicroversion(%q) error = %v", value, err)
		}
	}
	for _, value := range []string{"2.4", "1.99", "2", "two.five"} {
		if err := ValidateMicroversion(value); err == nil {
			t.Errorf("ValidateMicroversion(%q) unexpectedly succeeded", value)
		}
	}
}
