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

package cloud

import "testing"

func TestValidateGatewayIdentityReportsMissingFieldsInOrder(t *testing.T) {
	tests := []struct {
		name string
		id   Identity
		want string
	}{
		{name: "cluster", id: Identity{}, want: "cluster ID must not be empty"},
		{name: "controller", id: Identity{ClusterID: "cluster-a"}, want: "controller must not be empty"},
		{
			name: "namespace",
			id:   Identity{ClusterID: "cluster-a", Controller: "example.com/controller"},
			want: "Gateway namespace must not be empty",
		},
		{
			name: "name",
			id: Identity{
				ClusterID:        "cluster-a",
				Controller:       "example.com/controller",
				GatewayNamespace: "default",
			},
			want: "Gateway name must not be empty",
		},
		{
			name: "UID",
			id: Identity{
				ClusterID:        "cluster-a",
				Controller:       "example.com/controller",
				GatewayNamespace: "default",
				GatewayName:      "edge",
			},
			want: "Gateway UID must not be empty",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for range 20 {
				if err := ValidateGatewayIdentity(test.id); err == nil || err.Error() != test.want {
					t.Fatalf("ValidateGatewayIdentity() error = %v, want %q", err, test.want)
				}
			}
		})
	}
}

func TestValidateRouteIdentityReportsMissingFieldsInOrder(t *testing.T) {
	base := Identity{
		ClusterID:        "cluster-a",
		Controller:       "example.com/controller",
		GatewayNamespace: "default",
		GatewayName:      "edge",
		GatewayUID:       "gateway-uid",
	}
	tests := []struct {
		name string
		id   Identity
		want string
	}{
		{name: "namespace", id: base, want: "HTTPRoute namespace must not be empty"},
		{
			name: "name",
			id: func() Identity {
				id := base
				id.RouteNamespace = "default"
				return id
			}(),
			want: "HTTPRoute name must not be empty",
		},
		{
			name: "UID",
			id: func() Identity {
				id := base
				id.RouteNamespace = "default"
				id.RouteName = "api"
				return id
			}(),
			want: "HTTPRoute UID must not be empty",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for range 20 {
				if err := ValidateRouteIdentity(test.id); err == nil || err.Error() != test.want {
					t.Fatalf("ValidateRouteIdentity() error = %v, want %q", err, test.want)
				}
			}
		})
	}
}
