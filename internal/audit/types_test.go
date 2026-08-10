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

package audit

import "testing"

func TestScopeValidate(t *testing.T) {
	valid := Scope{
		ClusterID:          "cluster-a",
		ControllerName:     "example.com/gateway-api-openstack",
		OpenStackProjectID: "project-a",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	for _, field := range []string{"cluster", "controller", "project"} {
		t.Run(field, func(t *testing.T) {
			scope := valid
			switch field {
			case "cluster":
				scope.ClusterID = ""
			case "controller":
				scope.ControllerName = ""
			case "project":
				scope.OpenStackProjectID = ""
			}
			if err := scope.Validate(); err == nil {
				t.Fatal("Validate() error = nil, want invalid scope")
			}
		})
	}
}

func TestSortObjectReferences(t *testing.T) {
	objects := []ObjectReference{
		{APIVersion: "gateway.networking.k8s.io/v1", Kind: "HTTPRoute", Namespace: "z", Name: "route", UID: "route-uid"},
		{APIVersion: "gateway.networking.k8s.io/v1", Kind: "Gateway", Namespace: "a", Name: "gateway", UID: "gateway-uid"},
		{APIVersion: "gateway.networking.k8s.io/v1", Kind: "Gateway", Namespace: "a", Name: "gateway", UID: "gateway-uid"},
	}

	got := SortObjectReferences(objects)
	if len(got) != 2 {
		t.Fatalf("SortObjectReferences() length = %d, want 2", len(got))
	}
	if got[0].Kind != "Gateway" || got[1].Kind != "HTTPRoute" {
		t.Fatalf("SortObjectReferences() = %#v, want Gateway then HTTPRoute", got)
	}
	if objects[0].Kind != "HTTPRoute" {
		t.Fatal("SortObjectReferences() modified its input")
	}
}
