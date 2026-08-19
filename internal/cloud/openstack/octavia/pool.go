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

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
)

// ListPools lists and extracts pools matching opts.
func (s *Service) ListPools(ctx context.Context, opts pools.ListOpts) ([]pools.Pool, error) {
	pages, err := pools.List(s.client, opts).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pools: %w", err)
	}
	items, err := pools.ExtractPools(pages)
	if err != nil {
		return nil, fmt.Errorf("extract pools: %w", err)
	}
	return items, nil
}

// CreatePool creates one pool.
func (s *Service) CreatePool(ctx context.Context, opts pools.CreateOpts) (*pools.Pool, error) {
	return pools.Create(ctx, s.client, opts).Extract()
}

// UpdatePool updates one pool.
func (s *Service) UpdatePool(ctx context.Context, id string, opts pools.UpdateOpts) (*pools.Pool, error) {
	return pools.Update(ctx, s.client, id, opts).Extract()
}

// DeletePool deletes one pool.
func (s *Service) DeletePool(ctx context.Context, id string) error {
	return pools.Delete(ctx, s.client, id).ExtractErr()
}

// ListMembers lists and extracts the members beneath one pool.
func (s *Service) ListMembers(ctx context.Context, poolID string, opts pools.ListMembersOpts) ([]pools.Member, error) {
	pages, err := pools.ListMembers(s.client, poolID, opts).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pool members: %w", err)
	}
	items, err := pools.ExtractMembers(pages)
	if err != nil {
		return nil, fmt.Errorf("extract pool members: %w", err)
	}
	return items, nil
}

// CreateMember creates one member beneath a pool.
func (s *Service) CreateMember(ctx context.Context, poolID string, opts pools.CreateMemberOpts) (*pools.Member, error) {
	return pools.CreateMember(ctx, s.client, poolID, opts).Extract()
}

// UpdateMember updates one member beneath a pool.
func (s *Service) UpdateMember(ctx context.Context, poolID, memberID string, opts pools.UpdateMemberOpts) (*pools.Member, error) {
	return pools.UpdateMember(ctx, s.client, poolID, memberID, opts).Extract()
}

// DeleteMember deletes one member beneath a pool.
func (s *Service) DeleteMember(ctx context.Context, poolID, memberID string) error {
	return pools.DeleteMember(ctx, s.client, poolID, memberID).ExtractErr()
}
