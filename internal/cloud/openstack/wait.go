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
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack/octavia"
)

// WaitLoadBalancerActive retains the Phase 0 probe compatibility facade.
func WaitLoadBalancerActive(
	ctx context.Context,
	client *gophercloud.ServiceClient,
	loadBalancerID string,
	timeout time.Duration,
	pollInterval time.Duration,
) (*loadbalancers.LoadBalancer, error) {
	return octavia.WaitLoadBalancerActive(ctx, client, loadBalancerID, timeout, pollInterval)
}

// WaitLoadBalancerDeleted retains the Phase 0 probe compatibility facade.
func WaitLoadBalancerDeleted(
	ctx context.Context,
	client *gophercloud.ServiceClient,
	loadBalancerID string,
	timeout time.Duration,
	pollInterval time.Duration,
) error {
	return octavia.WaitLoadBalancerDeleted(ctx, client, loadBalancerID, timeout, pollInterval)
}

// IsNotFound reports whether an OpenStack request returned HTTP 404.
func IsNotFound(err error) bool {
	return octavia.IsNotFound(err)
}
