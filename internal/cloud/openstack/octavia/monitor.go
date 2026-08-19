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

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/monitors"
)

// ListMonitors lists and extracts health monitors matching opts.
func (s *Service) ListMonitors(ctx context.Context, opts monitors.ListOpts) ([]monitors.Monitor, error) {
	pages, err := monitors.List(s.client, opts).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list health monitors: %w", err)
	}
	items, err := monitors.ExtractMonitors(pages)
	if err != nil {
		return nil, fmt.Errorf("extract health monitors: %w", err)
	}
	return items, nil
}

// CreateMonitor creates one health monitor.
func (s *Service) CreateMonitor(ctx context.Context, opts monitors.CreateOpts) (*monitors.Monitor, error) {
	return monitors.Create(ctx, s.client, opts).Extract()
}

// UpdateMonitor updates one health monitor.
func (s *Service) UpdateMonitor(ctx context.Context, id string, opts monitors.UpdateOpts) (*monitors.Monitor, error) {
	return monitors.Update(ctx, s.client, id, opts).Extract()
}

// DeleteMonitor deletes one health monitor.
func (s *Service) DeleteMonitor(ctx context.Context, id string) error {
	return monitors.Delete(ctx, s.client, id).ExtractErr()
}
