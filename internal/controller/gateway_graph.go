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
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

const openStackResourceNamePrefix = "gateway-api-openstack"

func (r *GatewayReconciler) ensureGateway(ctx context.Context, expected *gatewayv1.Gateway) (cloud.GatewayResult, error) {
	if r.APIReader == nil {
		return cloud.GatewayResult{}, errAPIReaderRequired
	}
	release, err := acquireGatewayGraph(ctx, r.Coordinator, string(expected.UID))
	if err != nil {
		return cloud.GatewayResult{}, fmt.Errorf("acquire Gateway graph: %w", err)
	}
	defer release()

	var current gatewayv1.Gateway
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(expected), &current); err != nil {
		return cloud.GatewayResult{}, errors.Join(errGatewayChanged, client.IgnoreNotFound(err))
	}
	if current.UID != expected.UID || current.Generation != expected.Generation || !current.DeletionTimestamp.IsZero() {
		return cloud.GatewayResult{}, errGatewayChanged
	}
	var gatewayClass gatewayv1.GatewayClass
	if err := r.APIReader.Get(ctx, types.NamespacedName{Name: string(current.Spec.GatewayClassName)}, &gatewayClass); err != nil {
		return cloud.GatewayResult{}, errors.Join(errGatewayChanged, client.IgnoreNotFound(err))
	}
	if gatewayClass.Spec.ControllerName != r.Config.ControllerName || gatewayClass.Spec.ParametersRef != nil {
		return cloud.GatewayResult{}, errGatewayChanged
	}
	if !gatewayClassSupportsInstalledVersion(&gatewayClass) {
		return cloud.GatewayResult{}, errUnsupportedGatewayAPIVersion
	}
	if !controllerutil.ContainsFinalizer(&current, r.Config.gatewayFinalizer()) {
		return cloud.GatewayResult{}, errGatewayChanged
	}
	if err := validateGatewayBinding(r.Config, &current); err != nil {
		return cloud.GatewayResult{}, errors.Join(errGatewayChanged, err)
	}
	listener, validationErr := validateGateway(&current)
	if validationErr != nil || current.Annotations[r.Config.gatewayListenerPortAnnotation()] != strconv.Itoa(int(listener.Port)) {
		return cloud.GatewayResult{}, errGatewayChanged
	}
	if err := r.validateGatewayMutationOwnership(ctx, &current, true); err != nil {
		return cloud.GatewayResult{}, err
	}

	result, err := r.Provider.EnsureGateway(ctx, r.gatewaySpec(&current, listener))
	if err != nil {
		return cloud.GatewayResult{}, err
	}
	if err := result.Outcome.Validate(); err != nil {
		return cloud.GatewayResult{}, fmt.Errorf("validate Gateway provider outcome: %w", err)
	}
	return result, nil
}

func (r *GatewayReconciler) gatewaySpec(gateway *gatewayv1.Gateway, listener *gatewayv1.Listener) cloud.GatewaySpec {
	return cloud.GatewaySpec{
		Identity:          gatewayIdentity(r.Config, gateway),
		Provider:          r.Config.Provider,
		VIPSubnetID:       r.Config.VIPSubnetID,
		ExternalNetworkID: r.Config.ExternalNetworkID,
		ListenerName:      resourceDisplayName(gateway.Namespace, gateway.Name, string(listener.Name)),
		ListenerPort:      int(listener.Port),
	}
}

func resourceDisplayName(parts ...string) string {
	name := strings.Join(append([]string{openStackResourceNamePrefix}, parts...), "-")
	if len(name) > 200 {
		return name[:200]
	}
	return name
}
