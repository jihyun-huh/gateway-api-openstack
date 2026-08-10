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
	"fmt"
	"strings"

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

type loadBalancerPhase uint8

const (
	loadBalancerPhaseActive loadBalancerPhase = iota
	loadBalancerPhasePending
	loadBalancerPhaseAbsent
)

type loadBalancerObservation struct {
	loadBalancer *loadbalancers.LoadBalancer
	phase        loadBalancerPhase
	outcome      cloud.Outcome
}

func (p *Provider) observeLoadBalancerOnce(ctx context.Context, loadBalancerID string) (loadBalancerObservation, error) {
	loadBalancer, err := loadbalancers.Get(ctx, p.clients.LoadBalancer, loadBalancerID).Extract()
	if isNotFound(err) {
		return loadBalancerObservation{phase: loadBalancerPhaseAbsent}, nil
	}
	if err != nil {
		return loadBalancerObservation{}, fmt.Errorf("get load balancer: %w", err)
	}
	switch {
	case loadBalancer.ProvisioningStatus == "ACTIVE":
		return loadBalancerObservation{loadBalancer: loadBalancer, phase: loadBalancerPhaseActive, outcome: cloud.ReadyOutcome()}, nil
	case strings.HasPrefix(loadBalancer.ProvisioningStatus, "PENDING_"):
		message := fmt.Sprintf("Octavia load balancer is %s", loadBalancer.ProvisioningStatus)
		return loadBalancerObservation{
			loadBalancer: loadBalancer,
			phase:        loadBalancerPhasePending,
			outcome:      p.progressingOutcome(message),
		}, nil
	case loadBalancer.ProvisioningStatus == "ERROR":
		return loadBalancerObservation{}, cloud.NewProviderError(
			cloud.ErrorCategoryResourceFailure,
			fmt.Errorf("Octavia load balancer provisioning status is ERROR"),
		)
	default:
		return loadBalancerObservation{}, cloud.NewProviderError(
			cloud.ErrorCategoryResourceFailure,
			fmt.Errorf("Octavia load balancer has unknown provisioning status %q", loadBalancer.ProvisioningStatus),
		)
	}
}

func (p *Provider) progressingOutcome(message string) cloud.Outcome {
	return cloud.ProgressingOutcome(message, p.pollInterval)
}
