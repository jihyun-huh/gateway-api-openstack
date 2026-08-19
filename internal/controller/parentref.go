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

import gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

// IsGatewayParentRef reports whether a ParentReference targets a Gateway.
func IsGatewayParentRef(parent gatewayv1.ParentReference) bool {
	return (parent.Group == nil || *parent.Group == gatewayv1.Group(gatewayv1.GroupName)) &&
		(parent.Kind == nil || *parent.Kind == gatewayv1.Kind("Gateway"))
}

// ParentRefsEqual reports whether two ParentReferences select the same parent
// and section after applying their route namespace defaults.
func ParentRefsEqual(left gatewayv1.ParentReference, leftRouteNamespace string, right gatewayv1.ParentReference, rightRouteNamespace string) bool {
	leftGroup, rightGroup := gatewayv1.Group(gatewayv1.GroupName), gatewayv1.Group(gatewayv1.GroupName)
	if left.Group != nil {
		leftGroup = *left.Group
	}
	if right.Group != nil {
		rightGroup = *right.Group
	}
	leftKind, rightKind := gatewayv1.Kind("Gateway"), gatewayv1.Kind("Gateway")
	if left.Kind != nil {
		leftKind = *left.Kind
	}
	if right.Kind != nil {
		rightKind = *right.Kind
	}
	leftNamespace, rightNamespace := leftRouteNamespace, rightRouteNamespace
	if left.Namespace != nil {
		leftNamespace = string(*left.Namespace)
	}
	if right.Namespace != nil {
		rightNamespace = string(*right.Namespace)
	}
	return leftGroup == rightGroup && leftKind == rightKind && leftNamespace == rightNamespace && left.Name == right.Name &&
		stringPointerEqual(left.SectionName, right.SectionName) && int32PointerEqual(left.Port, right.Port)
}

// SameGatewayTarget reports whether two ParentReferences target the same
// Gateway, without comparing section names.
func SameGatewayTarget(left gatewayv1.ParentReference, leftRouteNamespace string, right gatewayv1.ParentReference, rightRouteNamespace string) bool {
	leftCopy, rightCopy := left, right
	leftCopy.SectionName, leftCopy.Port = nil, nil
	rightCopy.SectionName, rightCopy.Port = nil, nil
	return ParentRefsEqual(leftCopy, leftRouteNamespace, rightCopy, rightRouteNamespace)
}

func stringPointerEqual[T ~string](left, right *T) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func int32PointerEqual(left, right *gatewayv1.PortNumber) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}
