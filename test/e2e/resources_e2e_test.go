//go:build e2e

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

package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func (s *phase2Suite) createBackend(ctx context.Context) error {
	namespace := s.backendNamespace()
	s.createdNamespace = true
	if err := s.client.Create(ctx, namespace); err != nil {
		return fmt.Errorf("create isolated Namespace: %w", err)
	}
	s.namespaceUID = namespace.UID

	replicas := int32(2)
	deployment := s.backendDeployment(replicas)
	if err := s.client.Create(ctx, deployment); err != nil {
		return fmt.Errorf("create backend Deployment: %w", err)
	}

	service := s.backendService()
	if err := s.client.Create(ctx, service); err != nil {
		return fmt.Errorf("create backend NodePort Service: %w", err)
	}

	return s.waitForBackend(ctx, deployment, service, replicas)
}

func (s *phase2Suite) backendNamespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: s.config.Namespace,
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "gateway-api-openstack-e2e",
		},
		Annotations: map[string]string{runAnnotation: s.config.RunID},
	}}
}

func (s *phase2Suite) backendDeployment(replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: s.config.Namespace,
			Name:      backendName,
			Labels:    map[string]string{"app.kubernetes.io/name": backendName},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": backendName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": backendName}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  backendName,
					Image: s.config.BackendImage,
					Args:  []string{"netexec", fmt.Sprintf("--http-port=%d", backendPort)},
					Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: backendPort, Protocol: corev1.ProtocolTCP}},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromString("http")}},
						PeriodSeconds: 2,
					},
				}}},
			},
		},
	}
}

func (s *phase2Suite) backendService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: s.config.Namespace, Name: backendName},
		Spec: corev1.ServiceSpec{
			Type:                  corev1.ServiceTypeNodePort,
			ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyCluster,
			Selector:              map[string]string{"app.kubernetes.io/name": backendName},
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Protocol:   corev1.ProtocolTCP,
				Port:       listenerPort,
				TargetPort: intstr.FromString("http"),
			}},
		},
	}
}

func (s *phase2Suite) waitForBackend(ctx context.Context, deployment *appsv1.Deployment, service *corev1.Service, replicas int32) error {
	return wait.PollUntilContextCancel(ctx, s.config.PollInterval, true, func(ctx context.Context) (bool, error) {
		return s.backendReady(ctx, deployment, service, replicas)
	})
}

func (s *phase2Suite) backendReady(ctx context.Context, deployment *appsv1.Deployment, service *corev1.Service, replicas int32) (bool, error) {
	var currentDeployment appsv1.Deployment
	if err := s.client.Get(ctx, client.ObjectKeyFromObject(deployment), &currentDeployment); err != nil {
		return false, err
	}
	if !backendDeploymentReady(&currentDeployment, replicas) {
		return false, nil
	}
	var currentService corev1.Service
	if err := s.client.Get(ctx, client.ObjectKeyFromObject(service), &currentService); err != nil {
		return false, err
	}
	if len(currentService.Spec.Ports) != 1 || currentService.Spec.Ports[0].NodePort == 0 {
		return false, nil
	}
	return s.backendEndpointsReady(ctx, replicas)
}

func backendDeploymentReady(deployment *appsv1.Deployment, replicas int32) bool {
	return deployment.Status.ObservedGeneration == deployment.Generation &&
		deployment.Status.AvailableReplicas == replicas && deployment.Status.ReadyReplicas == replicas
}

func (s *phase2Suite) backendEndpointsReady(ctx context.Context, replicas int32) (bool, error) {
	var slices discoveryv1.EndpointSliceList
	if err := s.client.List(ctx, &slices,
		client.InNamespace(s.config.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: backendName},
	); err != nil {
		return false, err
	}
	return countReadyEndpoints(slices.Items) == int(replicas), nil
}

func countReadyEndpoints(slices []discoveryv1.EndpointSlice) int {
	readyEndpoints := 0
	for _, slice := range slices {
		for _, endpoint := range slice.Endpoints {
			if endpoint.Conditions.Ready != nil && *endpoint.Conditions.Ready {
				readyEndpoints++
			}
		}
	}
	return readyEndpoints
}

func (s *phase2Suite) createGatewayClass(ctx context.Context) error {
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:        s.gatewayClassName,
			Annotations: map[string]string{runAnnotation: s.config.RunID},
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController(s.config.ControllerName),
		},
	}
	s.createdClass = true
	if err := s.client.Create(ctx, gatewayClass); err != nil {
		return fmt.Errorf("create GatewayClass: %w", err)
	}
	s.gatewayClassUID = gatewayClass.UID
	return s.waitForGatewayClass(ctx)
}

func (s *phase2Suite) createGateway(ctx context.Context) error {
	same := gatewayv1.NamespacesFromSame
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   s.config.Namespace,
			Name:        gatewayName,
			Annotations: map[string]string{runAnnotation: s.config.RunID},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(s.gatewayClassName),
			Listeners: []gatewayv1.Listener{{
				Name:     gatewayv1.SectionName(listenerName),
				Protocol: gatewayv1.HTTPProtocolType,
				Port:     gatewayv1.PortNumber(listenerPort),
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Namespaces: &gatewayv1.RouteNamespaces{From: &same},
				},
			}},
		},
	}
	s.createdGateway = true
	if err := s.client.Create(ctx, gateway); err != nil {
		return fmt.Errorf("create Gateway: %w", err)
	}
	s.gatewayUID = gateway.UID
	return s.waitForGateway(ctx, 0)
}

func (s *phase2Suite) createHTTPRoute(ctx context.Context) error {
	sectionName := gatewayv1.SectionName(listenerName)
	pathType := gatewayv1.PathMatchPathPrefix
	path := "/"
	port := gatewayv1.PortNumber(listenerPort)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   s.config.Namespace,
			Name:        routeName,
			Annotations: map[string]string{runAnnotation: s.config.RunID},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{
				Name:        gatewayv1.ObjectName(gatewayName),
				SectionName: &sectionName,
			}}},
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(s.hostname)},
			Rules: []gatewayv1.HTTPRouteRule{{
				Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: &pathType, Value: &path}}},
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{Name: gatewayv1.ObjectName(backendName), Port: &port},
				}}},
			}},
		},
	}
	s.createdRoute = true
	if err := s.client.Create(ctx, route); err != nil {
		return fmt.Errorf("create HTTPRoute: %w", err)
	}
	s.routeUID = route.UID
	if err := s.waitForHTTPRoute(ctx); err != nil {
		return err
	}
	return s.waitForGateway(ctx, 1)
}

func (s *phase2Suite) waitForGatewayClass(ctx context.Context) error {
	return wait.PollUntilContextCancel(ctx, s.config.PollInterval, true, func(ctx context.Context) (bool, error) {
		var gatewayClass gatewayv1.GatewayClass
		if err := s.client.Get(ctx, client.ObjectKey{Name: s.gatewayClassName}, &gatewayClass); err != nil {
			return false, err
		}
		return exactCondition(gatewayClass.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted), metav1.ConditionTrue, string(gatewayv1.GatewayClassReasonAccepted), gatewayClass.Generation) &&
			exactCondition(gatewayClass.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion), metav1.ConditionTrue, string(gatewayv1.GatewayClassReasonSupportedVersion), gatewayClass.Generation), nil
	})
}

func (s *phase2Suite) waitForGateway(ctx context.Context, attachedRoutes int32) error {
	return wait.PollUntilContextCancel(ctx, s.config.PollInterval, true, func(ctx context.Context) (bool, error) {
		var gateway gatewayv1.Gateway
		if err := s.client.Get(ctx, client.ObjectKey{Namespace: s.config.Namespace, Name: gatewayName}, &gateway); err != nil {
			return false, err
		}
		return gatewayReady(&gateway, attachedRoutes), nil
	})
}

func gatewayReady(gateway *gatewayv1.Gateway, attachedRoutes int32) bool {
	if !gatewayHasReadyAddress(gateway) {
		return false
	}
	if !gatewayConditionsReady(gateway) {
		return false
	}
	if !gatewayListenerMatches(gateway, attachedRoutes) {
		return false
	}
	return gatewayListenerConditionsReady(gateway.Status.Listeners[0].Conditions, gateway.Generation)
}

func gatewayHasReadyAddress(gateway *gatewayv1.Gateway) bool {
	return len(gateway.Status.Addresses) == 1 && gateway.Status.Addresses[0].Type != nil &&
		*gateway.Status.Addresses[0].Type == gatewayv1.IPAddressType && strings.TrimSpace(gateway.Status.Addresses[0].Value) != ""
}

func gatewayConditionsReady(gateway *gatewayv1.Gateway) bool {
	return exactCondition(gateway.Status.Conditions, string(gatewayv1.GatewayConditionAccepted), metav1.ConditionTrue, string(gatewayv1.GatewayReasonAccepted), gateway.Generation) &&
		exactCondition(gateway.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed), metav1.ConditionTrue, string(gatewayv1.GatewayReasonProgrammed), gateway.Generation)
}

func gatewayListenerMatches(gateway *gatewayv1.Gateway, attachedRoutes int32) bool {
	return len(gateway.Status.Listeners) == 1 && gateway.Status.Listeners[0].Name == gatewayv1.SectionName(listenerName) &&
		gateway.Status.Listeners[0].AttachedRoutes == attachedRoutes
}

func gatewayListenerConditionsReady(conditions []metav1.Condition, generation int64) bool {
	return exactCondition(conditions, string(gatewayv1.ListenerConditionAccepted), metav1.ConditionTrue, string(gatewayv1.ListenerReasonAccepted), generation) &&
		exactCondition(conditions, string(gatewayv1.ListenerConditionResolvedRefs), metav1.ConditionTrue, string(gatewayv1.ListenerReasonResolvedRefs), generation) &&
		exactCondition(conditions, string(gatewayv1.ListenerConditionProgrammed), metav1.ConditionTrue, string(gatewayv1.ListenerReasonProgrammed), generation) &&
		exactCondition(conditions, string(gatewayv1.ListenerConditionConflicted), metav1.ConditionFalse, string(gatewayv1.ListenerReasonNoConflicts), generation)
}

func (s *phase2Suite) waitForHTTPRoute(ctx context.Context) error {
	return wait.PollUntilContextCancel(ctx, s.config.PollInterval, true, func(ctx context.Context) (bool, error) {
		var route gatewayv1.HTTPRoute
		if err := s.client.Get(ctx, client.ObjectKey{Namespace: s.config.Namespace, Name: routeName}, &route); err != nil {
			return false, err
		}
		var ownParents []gatewayv1.RouteParentStatus
		for _, parent := range route.Status.Parents {
			if parent.ControllerName == gatewayv1.GatewayController(s.config.ControllerName) {
				ownParents = append(ownParents, parent)
			}
		}
		if len(ownParents) != 1 || ownParents[0].ParentRef.Name != gatewayv1.ObjectName(gatewayName) ||
			ownParents[0].ParentRef.SectionName == nil || *ownParents[0].ParentRef.SectionName != gatewayv1.SectionName(listenerName) {
			return false, nil
		}
		controllerDomain, _, _ := strings.Cut(s.config.ControllerName, "/")
		conditions := ownParents[0].Conditions
		return exactCondition(conditions, string(gatewayv1.RouteConditionAccepted), metav1.ConditionTrue, string(gatewayv1.RouteReasonAccepted), route.Generation) &&
			exactCondition(conditions, string(gatewayv1.RouteConditionResolvedRefs), metav1.ConditionTrue, string(gatewayv1.RouteReasonResolvedRefs), route.Generation) &&
			exactCondition(conditions, controllerDomain+"/Programmed", metav1.ConditionTrue, "Programmed", route.Generation), nil
	})
}

func (s *phase2Suite) verifyExternalTraffic(ctx context.Context) error {
	expectedBody := "gateway-api-openstack-e2e-" + s.config.RunID
	return wait.PollUntilContextCancel(ctx, s.config.PollInterval, true, func(ctx context.Context) (bool, error) {
		address, err := s.gatewayAddress(ctx)
		if err != nil {
			return false, err
		}
		request, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			"http://"+net.JoinHostPort(address, "80")+"/echo?msg="+expectedBody,
			nil,
		)
		if err != nil {
			return false, fmt.Errorf("build external HTTP request: %w", err)
		}
		request.Host = s.hostname
		response, err := s.httpClient.Do(request)
		if err != nil {
			return false, nil
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil || len(body) > 4096 {
			return false, nil
		}
		return response.StatusCode == http.StatusOK && string(body) == expectedBody, nil
	})
}

func (s *phase2Suite) gatewayAddress(ctx context.Context) (string, error) {
	var gateway gatewayv1.Gateway
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: s.config.Namespace, Name: gatewayName}, &gateway); err != nil {
		return "", fmt.Errorf("read Gateway address: %w", err)
	}
	if len(gateway.Status.Addresses) != 1 || gateway.Status.Addresses[0].Type == nil ||
		*gateway.Status.Addresses[0].Type != gatewayv1.IPAddressType || strings.TrimSpace(gateway.Status.Addresses[0].Value) == "" {
		return "", fmt.Errorf("Gateway does not publish exactly one address")
	}
	return safeGatewayIPAddress(gateway.Status.Addresses[0].Value)
}

func safeGatewayIPAddress(value string) (string, error) {
	value = strings.TrimSpace(value)
	address := net.ParseIP(value)
	if address == nil || address.IsUnspecified() || address.IsLoopback() || address.IsMulticast() ||
		address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() {
		return "", fmt.Errorf("Gateway published an unsafe external address")
	}
	return value, nil
}

func exactCondition(conditions []metav1.Condition, conditionType string, status metav1.ConditionStatus, reason string, generation int64) bool {
	condition := meta.FindStatusCondition(conditions, conditionType)
	return condition != nil && condition.Status == status && condition.Reason == reason && condition.ObservedGeneration == generation
}

func TestSafeGatewayIPAddress(t *testing.T) {
	for _, value := range []string{"0.0.0.0", "127.0.0.1", "169.254.169.254", "224.0.0.1", "::", "::1", "fe80::1", "not-an-ip"} {
		if _, err := safeGatewayIPAddress(value); err == nil {
			t.Errorf("safeGatewayIPAddress(%q) accepted an unsafe address", value)
		}
	}
	for _, value := range []string{"192.0.2.10", "10.0.0.10", "2001:db8::10"} {
		if got, err := safeGatewayIPAddress(value); err != nil || got != value {
			t.Errorf("safeGatewayIPAddress(%q) = %q, %v", value, got, err)
		}
	}
}
