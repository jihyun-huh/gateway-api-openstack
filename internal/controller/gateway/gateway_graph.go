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

package gateway

import (
	"context"
	"fmt"
	"strings"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller/graph"
)

const openStackResourceNamePrefix = "gateway-api-openstack"

func (r *Reconciler) ensureGateway(ctx context.Context, expected *gatewayv1.Gateway) (cloud.GatewayResult, error) {
	if r.APIReader == nil {
		return cloud.GatewayResult{}, controller.ErrAPIReaderRequired
	}
	release, err := graph.Acquire(ctx, r.Coordinator, string(expected.UID))
	if err != nil {
		return cloud.GatewayResult{}, fmt.Errorf("acquire Gateway graph: %w", err)
	}
	defer release()

	snapshot, err := r.observeGatewayMutationSnapshot(ctx, expected, gatewayBindingRequired, "")
	if err != nil {
		return cloud.GatewayResult{}, err
	}
	desired := r.gatewaySpec(snapshot.gateway, snapshot.listener)
	if err := r.sealGatewayMutationSnapshot(ctx, snapshot); err != nil {
		return cloud.GatewayResult{}, err
	}

	result, err := r.Provider.EnsureGateway(ctx, desired)
	if err != nil {
		return cloud.GatewayResult{}, err
	}
	if err := result.Outcome.Validate(); err != nil {
		return cloud.GatewayResult{}, fmt.Errorf("validate Gateway provider outcome: %w", err)
	}
	return result, nil
}

func (r *Reconciler) gatewaySpec(gateway *gatewayv1.Gateway, listener *gatewayv1.Listener) cloud.GatewaySpec {
	return cloud.GatewaySpec{
		Identity:          controller.GatewayIdentity(r.Config, gateway),
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
