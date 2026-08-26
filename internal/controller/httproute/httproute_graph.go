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
	"context"
	"fmt"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller/graph"
)

type routeGraphResult struct {
	bindingCheckpoint bool
	outcome           cloud.Outcome
}

type routeBindingCheckpointError struct {
	err error
}

func (e *routeBindingCheckpointError) Error() string {
	return e.err.Error()
}

func (e *routeBindingCheckpointError) Unwrap() error {
	return e.err
}

func (r *Reconciler) ensureRouteGraph(
	ctx context.Context,
	expectedRoute *gatewayv1.HTTPRoute,
	expectedGateway *gatewayv1.Gateway,
) (routeGraphResult, error) {
	if r.APIReader == nil {
		return routeGraphResult{}, controller.ErrAPIReaderRequired
	}
	release, err := graph.Acquire(ctx, r.Coordinator, string(expectedGateway.UID))
	if err != nil {
		return routeGraphResult{}, fmt.Errorf("acquire Gateway graph: %w", err)
	}
	defer release()

	snapshot, err := r.buildRouteValidationSnapshot(
		ctx,
		expectedRoute,
		expectedGateway,
		routeSnapshotForEnsure,
	)
	if err != nil {
		return routeGraphResult{}, err
	}
	switch snapshot.disposition {
	case routeValidationGatewayPending:
		return routeGraphResult{}, nil
	case routeValidationBindingRequired:
		if _, err := r.bindRouteFromSnapshot(ctx, expectedRoute, expectedGateway, snapshot); err != nil {
			return routeGraphResult{}, &routeBindingCheckpointError{err: err}
		}
		return routeGraphResult{bindingCheckpoint: true}, nil
	case routeValidationReady:
	default:
		return routeGraphResult{}, fmt.Errorf("invalid HTTPRoute validation snapshot disposition %d", snapshot.disposition)
	}
	if err := r.sealRouteMutationSnapshot(ctx, snapshot, routeBindingRequired); err != nil {
		return routeGraphResult{}, err
	}

	result, err := r.Provider.EnsureRoute(ctx, snapshot.desired)
	if err != nil {
		return routeGraphResult{}, fmt.Errorf("ensure OpenStack resources for HTTPRoute: %w", err)
	}
	if err := result.Outcome.Validate(); err != nil {
		return routeGraphResult{}, fmt.Errorf("validate HTTPRoute provider outcome: %w", err)
	}
	return routeGraphResult{outcome: result.Outcome}, nil
}
