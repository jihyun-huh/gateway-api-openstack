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

package octavia

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
)

// ListLoadBalancers lists and extracts load balancers matching opts.
func (s *Service) ListLoadBalancers(ctx context.Context, opts loadbalancers.ListOpts) ([]loadbalancers.LoadBalancer, error) {
	pages, err := loadbalancers.List(s.client, opts).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list load balancers: %w", err)
	}
	items, err := loadbalancers.ExtractLoadBalancers(pages)
	if err != nil {
		return nil, fmt.Errorf("extract load balancers: %w", err)
	}
	return items, nil
}

// GetLoadBalancer gets one load balancer by ID.
func (s *Service) GetLoadBalancer(ctx context.Context, id string) (*loadbalancers.LoadBalancer, error) {
	return loadbalancers.Get(ctx, s.client, id).Extract()
}

// CreateLoadBalancer creates one load balancer.
func (s *Service) CreateLoadBalancer(ctx context.Context, opts loadbalancers.CreateOpts) (*loadbalancers.LoadBalancer, error) {
	return loadbalancers.Create(ctx, s.client, opts).Extract()
}

// UpdateLoadBalancer updates one load balancer.
func (s *Service) UpdateLoadBalancer(ctx context.Context, id string, opts loadbalancers.UpdateOpts) (*loadbalancers.LoadBalancer, error) {
	return loadbalancers.Update(ctx, s.client, id, opts).Extract()
}

// DeleteLoadBalancer deletes one load balancer without cascade options.
func (s *Service) DeleteLoadBalancer(ctx context.Context, id string) error {
	return loadbalancers.Delete(ctx, s.client, id, nil).ExtractErr()
}
