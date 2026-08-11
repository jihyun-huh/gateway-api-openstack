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
	"net/http"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
)

// WaitLoadBalancerActive waits until Octavia has completed the current
// asynchronous operation for a load balancer.
func WaitLoadBalancerActive(
	ctx context.Context,
	client *gophercloud.ServiceClient,
	loadBalancerID string,
	timeout time.Duration,
	pollInterval time.Duration,
) (*loadbalancers.LoadBalancer, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		loadBalancer, err := loadbalancers.Get(waitCtx, client, loadBalancerID).Extract()
		if err != nil {
			return nil, fmt.Errorf("get load balancer %s: %w", loadBalancerID, err)
		}
		switch loadBalancer.ProvisioningStatus {
		case "ACTIVE":
			return loadBalancer, nil
		case "ERROR":
			return nil, fmt.Errorf("load balancer %s entered ERROR", loadBalancerID)
		}

		select {
		case <-waitCtx.Done():
			return nil, fmt.Errorf("wait for load balancer %s to become ACTIVE: %w", loadBalancerID, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

// WaitLoadBalancerDeleted waits until Octavia returns 404 for a load balancer.
func WaitLoadBalancerDeleted(
	ctx context.Context,
	client *gophercloud.ServiceClient,
	loadBalancerID string,
	timeout time.Duration,
	pollInterval time.Duration,
) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		_, err := loadbalancers.Get(waitCtx, client, loadBalancerID).Extract()
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("get deleting load balancer %s: %w", loadBalancerID, err)
		}

		select {
		case <-waitCtx.Done():
			return fmt.Errorf("wait for load balancer %s deletion: %w", loadBalancerID, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

// IsNotFound reports whether an OpenStack request returned HTTP 404.
func IsNotFound(err error) bool {
	return gophercloud.ResponseCodeIs(err, http.StatusNotFound)
}

func isNotFound(err error) bool { return IsNotFound(err) }
