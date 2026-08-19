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

package httproute

import (
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestStatusForRouteBuildError(t *testing.T) {
	notFound := fmt.Errorf("get backend Service: %w", apierrors.NewNotFound(schema.GroupResource{Resource: "services"}, "backend"))
	status := statusForRouteBuildError(notFound)
	if status.resolved != metav1.ConditionFalse || status.resolvedReason != string(gatewayv1.RouteReasonBackendNotFound) {
		t.Fatalf("not-found status = %#v", status)
	}
	pending := statusForRouteBuildError(newRouteBuildError(routeErrorPending, "backend Service backend has no ready endpoints"))
	if pending.accepted != metav1.ConditionTrue || pending.programmedReason != "Pending" {
		t.Fatalf("pending status = %#v", pending)
	}
	untyped := statusForRouteBuildError(fmt.Errorf("backend Service backend has no ready endpoints"))
	if untyped.accepted != metav1.ConditionFalse || untyped.programmedReason != "Invalid" {
		t.Fatalf("untyped status = %#v", untyped)
	}
	rejected := rejectedRouteStatus(string(gatewayv1.RouteReasonUnsupportedValue), "unsupported")
	if rejected.resolved != metav1.ConditionUnknown || rejected.resolvedReason != string(gatewayv1.RouteReasonPending) {
		t.Fatalf("rejected status = %#v", rejected)
	}
}

func TestSupportedMatchDefaultsToRootPrefix(t *testing.T) {
	_, pathType, value, err := supportedMatch(nil)
	if err != nil || pathType != "PathPrefix" || value != "/" {
		t.Fatalf("supportedMatch(nil) = %q %q, %v", pathType, value, err)
	}
}
