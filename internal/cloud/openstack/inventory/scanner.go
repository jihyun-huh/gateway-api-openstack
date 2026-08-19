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

// Package inventory reads and classifies the OpenStack resources that may
// belong to this controller. It never mutates OpenStack resources.
package inventory

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/l7policies"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/monitors"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/jihyun-huh/gateway-api-openstack/internal/audit"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack/apierrors"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack/clients"
	openstackidentity "github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack/identity"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack/neutron"
	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack/octavia"
)

const (
	auditServiceOctavia = "Octavia"
	auditServiceNeutron = "Neutron"

	auditTypeLoadBalancer = "LoadBalancer"
	auditTypeListener     = "Listener"
	auditTypePool         = "Pool"
	auditTypeMember       = "Member"
	auditTypeMonitor      = "HealthMonitor"
	auditTypeL7Policy     = "L7Policy"
	auditTypeL7Rule       = "L7Rule"
	auditTypeFloatingIP   = "FloatingIP"

	roleLoadBalancer = openstackidentity.RoleLoadBalancer
	roleListener     = openstackidentity.RoleListener
	roleFloatingIP   = openstackidentity.RoleFloatingIP
	rolePool         = openstackidentity.RolePool
	roleMember       = openstackidentity.RoleMember
	roleMonitor      = openstackidentity.RoleMonitor
	rolePolicyExact  = openstackidentity.RolePolicyExact
	rolePolicyPrefix = openstackidentity.RolePolicyPrefix
	roleRulePath     = openstackidentity.RoleRulePath
	roleRuleHost     = openstackidentity.RoleRuleHost
)

var auditInventoryLimitations = []string{
	"Resources whose cluster or controller identity tags were removed cannot be found by the scoped Octavia queries.",
	"Members and L7 rules are scanned only beneath trusted pools and policies. A child whose complete ancestor chain lost its controller identity may not appear.",
	"An unmatched detached Floating IP cannot be attributed to a cluster or controller from its description digest and is omitted.",
}

// Scanner reads the authenticated project's ownership inventory without
// receiving any mutation authority from the controller.
type Scanner struct {
	octavia          octaviaReader
	neutron          neutronReader
	projectID        string
	operationTimeout time.Duration
}

type octaviaReader interface {
	ListLoadBalancers(context.Context, loadbalancers.ListOpts) ([]loadbalancers.LoadBalancer, error)
	ListListeners(context.Context, listeners.ListOpts) ([]listeners.Listener, error)
	ListPools(context.Context, pools.ListOpts) ([]pools.Pool, error)
	ListMembers(context.Context, string, pools.ListMembersOpts) ([]pools.Member, error)
	ListMonitors(context.Context, monitors.ListOpts) ([]monitors.Monitor, error)
	ListPolicies(context.Context, l7policies.ListOpts) ([]l7policies.L7Policy, error)
	ListRules(context.Context, string, l7policies.ListRulesOpts) ([]l7policies.Rule, error)
}

type neutronReader interface {
	ListFloatingIPs(context.Context, floatingips.ListOpts) ([]floatingips.FloatingIP, error)
}

// NewScanner constructs a read-only OpenStack ownership scanner.
func NewScanner(serviceClients clients.ServiceClients, operationTimeout time.Duration) *Scanner {
	if operationTimeout <= 0 {
		operationTimeout = 10 * time.Minute
	}
	var octaviaService octaviaReader
	if serviceClients.LoadBalancer != nil {
		octaviaService = octavia.NewService(serviceClients.LoadBalancer)
	}
	var neutronService neutronReader
	if serviceClients.Network != nil {
		neutronService = neutron.NewService(serviceClients.Network)
	}
	return &Scanner{
		octavia:          octaviaService,
		neutron:          neutronService,
		projectID:        serviceClients.ProjectID,
		operationTimeout: operationTimeout,
	}
}

var (
	_ audit.Scanner = (*Scanner)(nil)
	_ octaviaReader = (*octavia.Service)(nil)
	_ neutronReader = (*neutron.Service)(nil)
)

// Scan reads the controller's Octavia and Neutron inventory. It never sends a
// mutating request and does not treat a finding as authority to adopt or delete
// a resource.
func (p *Scanner) Scan(
	ctx context.Context,
	scope audit.Scope,
	records []audit.OwnershipRecord,
) (inventory audit.Inventory, retErr error) {
	defer func() {
		retErr = classifyOpenStackError(retErr)
	}()

	if err := scope.Validate(); err != nil {
		return audit.Inventory{}, cloud.NewProviderError(cloud.ErrorCategoryTerminalValidation, err)
	}
	if scope.OpenStackProjectID != p.projectID {
		return audit.Inventory{}, fmt.Errorf("%w: audit scope does not match the authenticated OpenStack project", cloud.ErrOwnershipConflict)
	}
	if p.octavia == nil || p.neutron == nil {
		return audit.Inventory{}, cloud.NewProviderError(
			cloud.ErrorCategoryTerminalValidation,
			fmt.Errorf("both Octavia and Neutron clients are required for an ownership inventory"),
		)
	}
	preparedRecords, err := prepareAuditRecords(scope, records)
	if err != nil {
		return audit.Inventory{}, cloud.NewProviderError(cloud.ErrorCategoryTerminalValidation, err)
	}

	ctx, cancel := p.operationContext(ctx)
	defer cancel()

	scopeTags := openstackidentity.ScopeTags(scope.ClusterID, scope.ControllerName)
	candidates := make(auditCandidateSet)
	if err := p.collectAuditLoadBalancers(ctx, candidates, scopeTags); err != nil {
		return audit.Inventory{}, err
	}
	loadBalancerIDs := candidates.scopedIDs(auditTypeLoadBalancer, scopeTags, p.projectID)
	if err := p.collectAuditListeners(ctx, candidates, scopeTags, loadBalancerIDs); err != nil {
		return audit.Inventory{}, err
	}
	if err := p.collectAuditPools(ctx, candidates, scopeTags, loadBalancerIDs); err != nil {
		return audit.Inventory{}, err
	}
	listenerIDs := candidates.scopedIDs(auditTypeListener, scopeTags, p.projectID)
	poolIDs := candidates.scopedIDs(auditTypePool, scopeTags, p.projectID)
	if err := p.collectAuditPolicies(ctx, candidates, scopeTags, listenerIDs); err != nil {
		return audit.Inventory{}, err
	}
	if err := p.collectAuditMembers(ctx, candidates, poolIDs); err != nil {
		return audit.Inventory{}, err
	}
	if err := p.collectAuditMonitors(ctx, candidates, scopeTags, poolIDs); err != nil {
		return audit.Inventory{}, err
	}
	policyIDs := candidates.scopedIDs(auditTypeL7Policy, scopeTags, p.projectID)
	if err := p.collectAuditRules(ctx, candidates, policyIDs); err != nil {
		return audit.Inventory{}, err
	}
	if err := p.collectAuditFloatingIPs(ctx, candidates, loadBalancerIDs, preparedRecords); err != nil {
		return audit.Inventory{}, err
	}

	return audit.Inventory{
		Resources:   classifyAuditCandidates(candidates, preparedRecords, scopeTags, scope.OpenStackProjectID),
		Limitations: append([]string(nil), auditInventoryLimitations...),
	}, nil
}

type preparedAuditRecord struct {
	record   audit.OwnershipRecord
	identity openstackidentity.Identity
	isRoute  bool
}

func prepareAuditRecords(scope audit.Scope, records []audit.OwnershipRecord) ([]preparedAuditRecord, error) {
	prepared := make([]preparedAuditRecord, 0, len(records))
	for index, record := range records {
		if record.Identity.ClusterID != scope.ClusterID || record.Identity.Controller != scope.ControllerName ||
			record.Identity.OpenStackProjectID != scope.OpenStackProjectID {
			return nil, fmt.Errorf("ownership record %d is outside the requested audit scope", index)
		}
		isRoute := record.Identity.RouteNamespace != "" || record.Identity.RouteName != "" || record.Identity.RouteUID != ""
		if isRoute {
			if err := cloud.ValidateRouteIdentity(record.Identity); err != nil {
				return nil, fmt.Errorf("validate ownership record %d: %w", index, err)
			}
		} else if err := cloud.ValidateGatewayIdentity(record.Identity); err != nil {
			return nil, fmt.Errorf("validate ownership record %d: %w", index, err)
		}
		identity, err := openstackidentity.NewIdentity(record.Identity)
		if err != nil {
			return nil, fmt.Errorf("prepare ownership record %d: %w", index, err)
		}
		record.Objects = audit.SortObjectReferences(record.Objects)
		prepared = append(prepared, preparedAuditRecord{record: record, identity: identity, isRoute: isRoute})
	}
	return prepared, nil
}

func (p *Scanner) operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, p.operationTimeout)
}

func classifyOpenStackError(err error) error {
	return apierrors.Classify(err)
}

func containsAll(actual, expected []string) bool {
	return openstackidentity.HasAllTags(actual, expected)
}

func tag(key, value string) string {
	return openstackidentity.Tag(key, value)
}

func parseIdentityTag(value string) (string, string, bool) {
	return openstackidentity.ParseTag(value)
}

func encode(value string) string {
	return openstackidentity.EncodeValue(value)
}

type auditCandidate struct {
	service            string
	resourceType       string
	id                 string
	projectID          string
	tenantID           string
	provider           string
	provisioningStatus string
	tags               []string
	description        string
	portID             string
	redirectPoolID     string
	reportedParents    map[string]struct{}
	observedParents    map[string]struct{}
	observationChanged bool
}

type auditCandidateSet map[string]*auditCandidate

func (s auditCandidateSet) add(candidate auditCandidate) {
	candidate.tags = sortedStrings(candidate.tags)
	key := candidate.service + "\x00" + candidate.resourceType + "\x00" + candidate.id
	existing := s[key]
	if existing == nil {
		candidate.reportedParents = cloneStringSet(candidate.reportedParents)
		candidate.observedParents = cloneStringSet(candidate.observedParents)
		s[key] = &candidate
		return
	}
	if existing.projectID != candidate.projectID || existing.tenantID != candidate.tenantID ||
		existing.provider != candidate.provider || !slices.Equal(existing.tags, candidate.tags) ||
		existing.description != candidate.description || existing.portID != candidate.portID ||
		existing.redirectPoolID != candidate.redirectPoolID {
		existing.observationChanged = true
	}
	if existing.provisioningStatus != candidate.provisioningStatus {
		existing.observationChanged = true
	}
	mergeStringSets(existing.reportedParents, candidate.reportedParents)
	mergeStringSets(existing.observedParents, candidate.observedParents)
}

func (s auditCandidateSet) scopedIDs(resourceType string, scopeTags []string, projectID string) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, candidate := range s {
		candidateProjectID := candidate.projectID
		if candidateProjectID == "" {
			candidateProjectID = candidate.tenantID
		}
		if candidate.resourceType == resourceType && candidateProjectID == projectID && containsAll(candidate.tags, scopeTags) {
			ids[candidate.id] = struct{}{}
		}
	}
	return ids
}

func (p *Scanner) collectAuditLoadBalancers(ctx context.Context, candidates auditCandidateSet, scopeTags []string) error {
	items, err := p.octavia.ListLoadBalancers(ctx, loadbalancers.ListOpts{
		ProjectID: p.projectID,
		Tags:      scopeTags,
	})
	if err != nil {
		return fmt.Errorf("list load balancers for ownership inventory: %w", err)
	}
	for _, item := range items {
		candidates.add(auditCandidate{
			service: auditServiceOctavia, resourceType: auditTypeLoadBalancer,
			id: item.ID, projectID: item.ProjectID, provider: item.Provider,
			provisioningStatus: item.ProvisioningStatus, tags: item.Tags,
			portID: item.VipPortID,
		})
	}
	return nil
}

func (p *Scanner) collectAuditListeners(
	ctx context.Context,
	candidates auditCandidateSet,
	scopeTags []string,
	loadBalancerIDs map[string]struct{},
) error {
	if err := p.listAuditListeners(ctx, candidates, listeners.ListOpts{
		ProjectID: p.projectID,
		Tags:      scopeTags,
	}, ""); err != nil {
		return err
	}
	for _, loadBalancerID := range sortedSetValues(loadBalancerIDs) {
		if err := p.listAuditListeners(ctx, candidates, listeners.ListOpts{
			ProjectID:      p.projectID,
			LoadbalancerID: loadBalancerID,
		}, loadBalancerID); err != nil {
			return err
		}
	}
	return nil
}

func (p *Scanner) listAuditListeners(
	ctx context.Context,
	candidates auditCandidateSet,
	opts listeners.ListOpts,
	observedParent string,
) error {
	items, err := p.octavia.ListListeners(ctx, opts)
	if err != nil {
		return fmt.Errorf("list listeners for ownership inventory: %w", err)
	}
	for _, item := range items {
		parents := make(map[string]struct{}, len(item.Loadbalancers))
		for _, parent := range item.Loadbalancers {
			parents[parent.ID] = struct{}{}
		}
		candidates.add(auditCandidate{
			service: auditServiceOctavia, resourceType: auditTypeListener,
			id: item.ID, projectID: item.ProjectID,
			provisioningStatus: item.ProvisioningStatus, tags: item.Tags,
			reportedParents: parents, observedParents: singletonStringSet(observedParent),
		})
	}
	return nil
}

func (p *Scanner) collectAuditPools(
	ctx context.Context,
	candidates auditCandidateSet,
	scopeTags []string,
	loadBalancerIDs map[string]struct{},
) error {
	if err := p.listAuditPools(ctx, candidates, pools.ListOpts{
		ProjectID: p.projectID,
		Tags:      scopeTags,
	}, ""); err != nil {
		return err
	}
	for _, loadBalancerID := range sortedSetValues(loadBalancerIDs) {
		if err := p.listAuditPools(ctx, candidates, pools.ListOpts{
			ProjectID:      p.projectID,
			LoadbalancerID: loadBalancerID,
		}, loadBalancerID); err != nil {
			return err
		}
	}
	return nil
}

func (p *Scanner) listAuditPools(
	ctx context.Context,
	candidates auditCandidateSet,
	opts pools.ListOpts,
	observedParent string,
) error {
	items, err := p.octavia.ListPools(ctx, opts)
	if err != nil {
		return fmt.Errorf("list pools for ownership inventory: %w", err)
	}
	for _, item := range items {
		parents := make(map[string]struct{}, len(item.Loadbalancers))
		for _, parent := range item.Loadbalancers {
			parents[parent.ID] = struct{}{}
		}
		candidates.add(auditCandidate{
			service: auditServiceOctavia, resourceType: auditTypePool,
			id: item.ID, projectID: item.ProjectID,
			provisioningStatus: item.ProvisioningStatus, tags: item.Tags,
			reportedParents: parents, observedParents: singletonStringSet(observedParent),
		})
	}
	return nil
}

func (p *Scanner) collectAuditPolicies(
	ctx context.Context,
	candidates auditCandidateSet,
	scopeTags []string,
	listenerIDs map[string]struct{},
) error {
	items, err := p.octavia.ListPolicies(ctx, l7policies.ListOpts{
		ProjectID: p.projectID,
	})
	if err != nil {
		return fmt.Errorf("list L7 policies for ownership inventory: %w", err)
	}
	for _, item := range items {
		_, descendant := listenerIDs[item.ListenerID]
		if !containsAll(item.Tags, scopeTags) && !descendant {
			continue
		}
		candidates.add(auditCandidate{
			service: auditServiceOctavia, resourceType: auditTypeL7Policy,
			id: item.ID, projectID: item.ProjectID,
			provisioningStatus: item.ProvisioningStatus, tags: item.Tags,
			redirectPoolID:  item.RedirectPoolID,
			reportedParents: singletonStringSet(item.ListenerID),
			observedParents: singletonStringSetIf(descendant, item.ListenerID),
		})
	}
	return nil
}

func (p *Scanner) collectAuditMembers(ctx context.Context, candidates auditCandidateSet, poolIDs map[string]struct{}) error {
	for _, poolID := range sortedSetValues(poolIDs) {
		items, err := p.octavia.ListMembers(ctx, poolID, pools.ListMembersOpts{
			ProjectID: p.projectID,
		})
		if err != nil {
			return fmt.Errorf("list members beneath pool %s for ownership inventory: %w", poolID, err)
		}
		for _, item := range items {
			candidates.add(auditCandidate{
				service: auditServiceOctavia, resourceType: auditTypeMember,
				id: item.ID, projectID: item.ProjectID,
				provisioningStatus: item.ProvisioningStatus, tags: item.Tags,
				reportedParents: singletonStringSet(item.PoolID),
				observedParents: singletonStringSet(poolID),
			})
		}
	}
	return nil
}

func (p *Scanner) collectAuditMonitors(
	ctx context.Context,
	candidates auditCandidateSet,
	scopeTags []string,
	poolIDs map[string]struct{},
) error {
	if err := p.listAuditMonitors(ctx, candidates, monitors.ListOpts{
		ProjectID: p.projectID,
		Tags:      scopeTags,
	}, ""); err != nil {
		return err
	}
	for _, poolID := range sortedSetValues(poolIDs) {
		if err := p.listAuditMonitors(ctx, candidates, monitors.ListOpts{
			ProjectID: p.projectID,
			PoolID:    poolID,
		}, poolID); err != nil {
			return err
		}
	}
	return nil
}

func (p *Scanner) listAuditMonitors(
	ctx context.Context,
	candidates auditCandidateSet,
	opts monitors.ListOpts,
	observedParent string,
) error {
	items, err := p.octavia.ListMonitors(ctx, opts)
	if err != nil {
		return fmt.Errorf("list health monitors for ownership inventory: %w", err)
	}
	for _, item := range items {
		parents := make(map[string]struct{}, len(item.Pools))
		for _, parent := range item.Pools {
			parents[parent.ID] = struct{}{}
		}
		candidates.add(auditCandidate{
			service: auditServiceOctavia, resourceType: auditTypeMonitor,
			id: item.ID, projectID: item.ProjectID,
			provisioningStatus: item.ProvisioningStatus, tags: item.Tags,
			reportedParents: parents, observedParents: singletonStringSet(observedParent),
		})
	}
	return nil
}

func (p *Scanner) collectAuditRules(ctx context.Context, candidates auditCandidateSet, policyIDs map[string]struct{}) error {
	for _, policyID := range sortedSetValues(policyIDs) {
		items, err := p.octavia.ListRules(ctx, policyID, l7policies.ListRulesOpts{
			ProjectID: p.projectID,
		})
		if err != nil {
			return fmt.Errorf("list rules beneath L7 policy %s for ownership inventory: %w", policyID, err)
		}
		for _, item := range items {
			candidates.add(auditCandidate{
				service: auditServiceOctavia, resourceType: auditTypeL7Rule,
				id: item.ID, projectID: item.ProjectID,
				provisioningStatus: item.ProvisioningStatus, tags: item.Tags,
				observedParents: singletonStringSet(policyID),
			})
		}
	}
	return nil
}

func (p *Scanner) collectAuditFloatingIPs(
	ctx context.Context,
	candidates auditCandidateSet,
	loadBalancerIDs map[string]struct{},
	records []preparedAuditRecord,
) error {
	loadBalancersByPort := make(map[string]map[string]struct{})
	for _, candidate := range candidates {
		if candidate.resourceType == auditTypeLoadBalancer && candidate.portID != "" {
			if _, scoped := loadBalancerIDs[candidate.id]; scoped {
				if loadBalancersByPort[candidate.portID] == nil {
					loadBalancersByPort[candidate.portID] = make(map[string]struct{})
				}
				loadBalancersByPort[candidate.portID][candidate.id] = struct{}{}
			}
		}
	}
	items, err := p.neutron.ListFloatingIPs(ctx, floatingips.ListOpts{
		ProjectID: p.projectID,
	})
	if err != nil {
		return fmt.Errorf("list Floating IPs for ownership inventory: %w", err)
	}
	for _, item := range items {
		parents := loadBalancersByPort[item.PortID]
		if len(parents) == 0 && !auditDescriptionMatchesRecord(item.Description, records) {
			continue
		}
		candidates.add(auditCandidate{
			service: auditServiceNeutron, resourceType: auditTypeFloatingIP,
			id: item.ID, projectID: item.ProjectID, tenantID: item.TenantID,
			description: item.Description, portID: item.PortID,
			reportedParents: cloneStringSet(parents),
		})
	}
	return nil
}

func auditDescriptionMatchesRecord(description string, records []preparedAuditRecord) bool {
	if !strings.HasPrefix(description, "gateway-api-openstack; identity-sha256=") {
		return false
	}
	for _, record := range records {
		if record.identity.MatchesGatewayDescription(description, roleFloatingIP) {
			return true
		}
	}
	return false
}

func sortedStrings(values []string) []string {
	result := append([]string(nil), values...)
	slices.Sort(result)
	return result
}

func singletonStringSet(value string) map[string]struct{} {
	if value == "" {
		return map[string]struct{}{}
	}
	return map[string]struct{}{value: {}}
}

func singletonStringSetIf(include bool, value string) map[string]struct{} {
	if !include {
		return map[string]struct{}{}
	}
	return singletonStringSet(value)
}

func cloneStringSet(values map[string]struct{}) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	mergeStringSets(result, values)
	return result
}

func mergeStringSets(destination, source map[string]struct{}) {
	for value := range source {
		destination[value] = struct{}{}
	}
}

func sortedSetValues(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}
