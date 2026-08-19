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

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
)

// ListListeners lists and extracts listeners matching opts.
func (s *Service) ListListeners(ctx context.Context, opts listeners.ListOpts) ([]listeners.Listener, error) {
	pages, err := listeners.List(s.client, opts).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list listeners: %w", err)
	}
	items, err := listeners.ExtractListeners(pages)
	if err != nil {
		return nil, fmt.Errorf("extract listeners: %w", err)
	}
	return items, nil
}

// CreateListener creates one listener.
func (s *Service) CreateListener(ctx context.Context, opts listeners.CreateOpts) (*listeners.Listener, error) {
	return listeners.Create(ctx, s.client, opts).Extract()
}

// UpdateListener updates one listener.
func (s *Service) UpdateListener(ctx context.Context, id string, opts listeners.UpdateOpts) (*listeners.Listener, error) {
	return listeners.Update(ctx, s.client, id, opts).Extract()
}

// DeleteListener deletes one listener.
func (s *Service) DeleteListener(ctx context.Context, id string) error {
	return listeners.Delete(ctx, s.client, id).ExtractErr()
}
