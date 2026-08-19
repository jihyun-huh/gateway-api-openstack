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

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/l7policies"
)

// ListPolicies lists and extracts L7 policies matching opts.
func (s *Service) ListPolicies(ctx context.Context, opts l7policies.ListOpts) ([]l7policies.L7Policy, error) {
	pages, err := l7policies.List(s.client, opts).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list L7 policies: %w", err)
	}
	items, err := l7policies.ExtractL7Policies(pages)
	if err != nil {
		return nil, fmt.Errorf("extract L7 policies: %w", err)
	}
	return items, nil
}

// CreatePolicy creates one L7 policy.
func (s *Service) CreatePolicy(ctx context.Context, opts l7policies.CreateOpts) (*l7policies.L7Policy, error) {
	return l7policies.Create(ctx, s.client, opts).Extract()
}

// UpdatePolicy updates one L7 policy.
func (s *Service) UpdatePolicy(ctx context.Context, id string, opts l7policies.UpdateOpts) (*l7policies.L7Policy, error) {
	return l7policies.Update(ctx, s.client, id, opts).Extract()
}

// DeletePolicy deletes one L7 policy.
func (s *Service) DeletePolicy(ctx context.Context, id string) error {
	return l7policies.Delete(ctx, s.client, id).ExtractErr()
}

// ListRules lists and extracts L7 rules beneath one policy.
func (s *Service) ListRules(ctx context.Context, policyID string, opts l7policies.ListRulesOpts) ([]l7policies.Rule, error) {
	pages, err := l7policies.ListRules(s.client, policyID, opts).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list L7 rules: %w", err)
	}
	items, err := l7policies.ExtractRules(pages)
	if err != nil {
		return nil, fmt.Errorf("extract L7 rules: %w", err)
	}
	return items, nil
}

// CreateRule creates one L7 rule beneath a policy.
func (s *Service) CreateRule(ctx context.Context, policyID string, opts l7policies.CreateRuleOpts) (*l7policies.Rule, error) {
	return l7policies.CreateRule(ctx, s.client, policyID, opts).Extract()
}

// UpdateRule updates one L7 rule beneath a policy.
func (s *Service) UpdateRule(ctx context.Context, policyID, ruleID string, opts l7policies.UpdateRuleOpts) (*l7policies.Rule, error) {
	return l7policies.UpdateRule(ctx, s.client, policyID, ruleID, opts).Extract()
}

// DeleteRule deletes one L7 rule beneath a policy.
func (s *Service) DeleteRule(ctx context.Context, policyID, ruleID string) error {
	return l7policies.DeleteRule(ctx, s.client, policyID, ruleID).ExtractErr()
}
