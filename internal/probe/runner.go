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

package probe

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/l7policies"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/monitors"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/providers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
)

const (
	roleLoadBalancer = "loadbalancer"
	roleListener     = "listener"
	rolePool         = "pool"
	roleMember       = "member"
	roleMonitor      = "health-monitor"
	roleL7Policy     = "l7-policy"
	roleL7Rule       = "l7-rule"
	roleFloatingIP   = "floating-ip"
)

type runner struct {
	config  Config
	clients serviceClients
	state   State
	report  *Report
}

// Run executes a complete create, update, inspect, traffic, and delete cycle.
func Run(ctx context.Context, cfg Config) (report *Report, runErr error) {
	report = newReport(cfg)
	currentRunner := &runner{
		config: cfg,
		state:  NewState(cfg),
		report: report,
	}

	defer func() {
		if !cfg.KeepResources && currentRunner.state.LoadBalancerID != "" {
			report.CleanupAttempted = true
			cleanupErr := cleanupWithClients(
				ctx,
				currentRunner.clients,
				currentRunner.state,
				cfg.StateFile,
				cfg.OperationTimeout,
				cfg.PollInterval,
			)
			report.CleanupSucceeded = cleanupErr == nil
			if cleanupErr != nil {
				runErr = errors.Join(runErr, fmt.Errorf("automatic cleanup: %w", cleanupErr))
			}
		}

		report.CompletedAt = time.Now().UTC()
		if runErr != nil {
			report.Outcome = "failed"
			report.Error = runErr.Error()
		} else {
			report.Outcome = "succeeded"
		}
		if saveErr := SaveReport(cfg.ReportFile, report); saveErr != nil {
			runErr = errors.Join(runErr, saveErr)
		}
	}()

	if err := requireAuthenticationEnvironment(); err != nil {
		return report, err
	}

	started := time.Now()
	clients, err := newServiceClients(
		ctx,
		cfg.Region,
		cfg.OctaviaMicroversion,
		cfg.AllowInsecureOpenStack,
	)
	currentRunner.record("authenticate", "openstack", "", started, err)
	if err != nil {
		return report, err
	}
	currentRunner.clients = clients

	if err := CreateState(cfg.StateFile, currentRunner.state); err != nil {
		return report, err
	}

	currentRunner.listLoadbalancerProviders(ctx)
	if err := currentRunner.createAndUpdateResources(ctx); err != nil {
		return report, err
	}
	if err := currentRunner.inspectResources(ctx); err != nil {
		return report, err
	}
	if !cfg.SkipTrafficCheck {
		if err := currentRunner.checkTraffic(ctx); err != nil {
			return report, err
		}
	}
	return report, nil
}

func (r *runner) listLoadbalancerProviders(ctx context.Context) {
	started := time.Now()
	pages, err := providers.List(r.clients.loadBalancer, providers.ListOpts{}).AllPages(ctx)
	if err == nil {
		var available []providers.Provider
		available, err = providers.ExtractProviders(pages)
		for _, provider := range available {
			r.report.Providers = append(r.report.Providers, ProviderRecord{
				Name:        provider.Name,
				Description: provider.Description,
			})
		}
	}
	r.record("list", "octavia providers", "", started, err)
}

func (r *runner) record(operation, resource, resourceID string, started time.Time, err error) {
	step := StepRecord{
		Operation:  operation,
		Resource:   resource,
		ResourceID: resourceID,
		Duration:   time.Since(started),
		Succeeded:  err == nil,
	}
	if err != nil {
		step.Error = err.Error()
	}
	r.report.Steps = append(r.report.Steps, step)
}

func (r *runner) createAndUpdateResources(ctx context.Context) error {
	identity := r.state.Identity

	loadBalancerTags, err := identity.Tags(roleLoadBalancer)
	if err != nil {
		return err
	}
	started := time.Now()
	loadBalancer, err := loadbalancers.Create(ctx, r.clients.loadBalancer, loadbalancers.CreateOpts{
		Name:         r.resourceName(roleLoadBalancer),
		Description:  identity.Description(roleLoadBalancer) + "; phase=created",
		VipSubnetID:  r.config.VIPSubnetID,
		Provider:     r.config.Provider,
		AdminStateUp: boolPointer(true),
		Tags:         loadBalancerTags,
	}).Extract()
	r.record("create", roleLoadBalancer, resourceID(loadBalancer), started, err)
	if err != nil {
		return fmt.Errorf("create load balancer: %w", err)
	}
	r.state.LoadBalancerID = loadBalancer.ID
	r.state.VIPPortID = loadBalancer.VipPortID
	r.report.Resources.LoadBalancerID = loadBalancer.ID
	r.report.Resources.VIPAddress = loadBalancer.VipAddress
	if err := r.saveState(); err != nil {
		return err
	}
	if _, err := r.waitActive(ctx); err != nil {
		return err
	}

	verifiedDescription := identity.Description(roleLoadBalancer) + "; phase=verified"
	started = time.Now()
	_, err = loadbalancers.Update(ctx, r.clients.loadBalancer, loadBalancer.ID, loadbalancers.UpdateOpts{
		Description: &verifiedDescription,
		Tags:        &loadBalancerTags,
	}).Extract()
	r.record("update", roleLoadBalancer, loadBalancer.ID, started, err)
	if err != nil {
		return fmt.Errorf("update load balancer: %w", err)
	}
	if _, err := r.waitActive(ctx); err != nil {
		return err
	}

	if r.config.ExternalNetworkID != "" {
		if err := r.createAndUpdateFloatingIP(ctx); err != nil {
			return err
		}
	}
	if err := r.createAndUpdateListener(ctx); err != nil {
		return err
	}
	if err := r.createAndUpdatePool(ctx); err != nil {
		return err
	}
	if err := r.createAndUpdateMembers(ctx); err != nil {
		return err
	}
	if err := r.createAndUpdateHealthMonitor(ctx); err != nil {
		return err
	}
	if err := r.createAndUpdateL7Policy(ctx); err != nil {
		return err
	}
	if err := r.createAndUpdateL7Rule(ctx); err != nil {
		return err
	}
	return nil

}

func (r *runner) createAndUpdateFloatingIP(ctx context.Context) error {
	createdDescription := r.state.Identity.Description(roleFloatingIP) + "; phase=created"
	started := time.Now()

	floatingIP, err := floatingips.Create(ctx, r.clients.network, floatingips.CreateOpts{
		Description:       createdDescription,
		FloatingNetworkID: r.config.ExternalNetworkID,
		PortID:            r.state.VIPPortID,
	}).Extract()
	r.record("create", roleFloatingIP, resourceID(floatingIP), started, err)
	if err != nil {
		return fmt.Errorf("create Floating IP: %w", err)
	}

	r.state.FloatingIPID = floatingIP.ID
	r.report.Resources.FloatingIPID = floatingIP.ID
	r.report.Resources.FloatingIP = floatingIP.FloatingIP
	if err := r.saveState(); err != nil {
		return err
	}

	verifiedDescription := r.state.Identity.Description(roleFloatingIP) + "; phase=verified"

	started = time.Now()
	_, err = floatingips.Update(ctx, r.clients.network, floatingIP.ID, floatingips.UpdateOpts{
		Description: &verifiedDescription,
	}).Extract()
	r.record("update", roleFloatingIP, floatingIP.ID, started, err)
	if err != nil {
		return fmt.Errorf("update Floating IP: %w", err)
	}
	return nil
}

func (r *runner) createAndUpdateListener(ctx context.Context) error {
	tags, err := r.state.Identity.Tags(roleListener)
	if err != nil {
		return err
	}
	started := time.Now()
	listener, err := listeners.Create(ctx, r.clients.loadBalancer, listeners.CreateOpts{
		LoadbalancerID: r.state.LoadBalancerID,
		Protocol:       listeners.ProtocolHTTP,
		ProtocolPort:   r.config.ListenerPort,
		Name:           r.resourceName(roleListener),
		Description:    r.state.Identity.Description(roleListener) + "; phase=created",
		AdminStateUp:   boolPointer(true),
		Tags:           tags,
	}).Extract()
	r.record("create", roleListener, resourceID(listener), started, err)
	if err != nil {
		return fmt.Errorf("create listener: %w", err)
	}
	r.state.ListenerID = listener.ID
	r.report.Resources.ListenerID = listener.ID
	if err := r.saveState(); err != nil {
		return err
	}
	if _, err := r.waitActive(ctx); err != nil {
		return err
	}

	description := r.state.Identity.Description(roleListener) + "; phase=verified"
	started = time.Now()
	_, err = listeners.Update(ctx, r.clients.loadBalancer, listener.ID, listeners.UpdateOpts{
		Description: &description,
		Tags:        &tags,
	}).Extract()
	r.record("update", roleListener, listener.ID, started, err)
	if err != nil {
		return fmt.Errorf("update listener: %w", err)
	}
	_, err = r.waitActive(ctx)
	return err
}

func (r *runner) createAndUpdatePool(ctx context.Context) error {
	tags, err := r.state.Identity.Tags(rolePool)
	if err != nil {
		return err
	}
	started := time.Now()
	pool, err := pools.Create(ctx, r.clients.loadBalancer, pools.CreateOpts{
		LBMethod:       pools.LBMethodRoundRobin,
		Protocol:       pools.ProtocolHTTP,
		LoadbalancerID: r.state.LoadBalancerID,
		Name:           r.resourceName(rolePool),
		Description:    r.state.Identity.Description(rolePool) + "; phase=created",
		AdminStateUp:   boolPointer(true),
		Tags:           tags,
	}).Extract()
	r.record("create", rolePool, resourceID(pool), started, err)
	if err != nil {
		return fmt.Errorf("create pool: %w", err)
	}
	r.state.PoolID = pool.ID
	r.report.Resources.PoolID = pool.ID
	if err := r.saveState(); err != nil {
		return err
	}
	if _, err := r.waitActive(ctx); err != nil {
		return err
	}

	description := r.state.Identity.Description(rolePool) + "; phase=verified"
	started = time.Now()
	_, err = pools.Update(ctx, r.clients.loadBalancer, pool.ID, pools.UpdateOpts{
		Description: &description,
		Tags:        &tags,
	}).Extract()
	r.record("update", rolePool, pool.ID, started, err)
	if err != nil {
		return fmt.Errorf("update pool: %w", err)
	}
	_, err = r.waitActive(ctx)
	return err
}

func (r *runner) createAndUpdateMembers(ctx context.Context) error {
	for index, address := range r.config.MemberAddresses {
		tags, err := r.state.Identity.Tags(roleMember)
		if err != nil {
			return err
		}
		started := time.Now()
		member, err := pools.CreateMember(ctx, r.clients.loadBalancer, r.state.PoolID, pools.CreateMemberOpts{
			Address:      address,
			ProtocolPort: r.config.MemberPort,
			Name:         fmt.Sprintf("%s-%d", r.resourceName(roleMember), index),
			SubnetID:     r.config.MemberSubnetID,
			AdminStateUp: boolPointer(true),
			Tags:         tags,
		}).Extract()
		r.record("create", roleMember, resourceID(member), started, err)
		if err != nil {
			return fmt.Errorf("create member %s: %w", address, err)
		}
		r.state.MemberIDs = append(r.state.MemberIDs, member.ID)
		r.report.Resources.MemberIDs = append(r.report.Resources.MemberIDs, member.ID)
		if err := r.saveState(); err != nil {
			return err
		}
		if _, err := r.waitActive(ctx); err != nil {
			return err
		}

		updatedName := fmt.Sprintf("%s-%d-verified", r.resourceName(roleMember), index)
		started = time.Now()
		_, err = pools.UpdateMember(
			ctx,
			r.clients.loadBalancer,
			r.state.PoolID,
			member.ID,
			pools.UpdateMemberOpts{
				Name: &updatedName,
				Tags: tags,
			},
		).Extract()
		r.record("update", roleMember, member.ID, started, err)
		if err != nil {
			return fmt.Errorf("update member %s: %w", address, err)
		}
		if _, err := r.waitActive(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) createAndUpdateHealthMonitor(ctx context.Context) error {
	tags, err := r.state.Identity.Tags(roleMonitor)
	if err != nil {
		return err
	}
	started := time.Now()
	monitor, err := monitors.Create(ctx, r.clients.loadBalancer, monitors.CreateOpts{
		PoolID:         r.state.PoolID,
		Type:           monitors.TypeHTTP,
		Delay:          10,
		Timeout:        5,
		MaxRetries:     3,
		MaxRetriesDown: 3,
		URLPath:        r.config.HealthPath,
		HTTPMethod:     http.MethodGet,
		ExpectedCodes:  "200-399",
		Name:           r.resourceName(roleMonitor),
		AdminStateUp:   boolPointer(true),
		Tags:           tags,
	}).Extract()
	r.record("create", roleMonitor, resourceID(monitor), started, err)
	if err != nil {
		return fmt.Errorf("create health monitor: %w", err)
	}
	r.state.MonitorID = monitor.ID
	r.report.Resources.MonitorID = monitor.ID
	if err := r.saveState(); err != nil {
		return err
	}
	if _, err := r.waitActive(ctx); err != nil {
		return err
	}

	updatedName := r.resourceName(roleMonitor) + "-verified"
	started = time.Now()
	_, err = monitors.Update(ctx, r.clients.loadBalancer, monitor.ID, monitors.UpdateOpts{
		Name: &updatedName,
		Tags: tags,
	}).Extract()
	r.record("update", roleMonitor, monitor.ID, started, err)
	if err != nil {
		return fmt.Errorf("update health monitor: %w", err)
	}
	_, err = r.waitActive(ctx)
	return err
}

func (r *runner) createAndUpdateL7Policy(ctx context.Context) error {
	tags, err := r.state.Identity.Tags(roleL7Policy)
	if err != nil {
		return err
	}
	started := time.Now()
	policy, err := l7policies.Create(ctx, r.clients.loadBalancer, l7policies.CreateOpts{
		Name:           r.resourceName(roleL7Policy),
		ListenerID:     r.state.ListenerID,
		Action:         l7policies.ActionRedirectToPool,
		Position:       1,
		Description:    r.state.Identity.Description(roleL7Policy) + "; phase=created",
		RedirectPoolID: r.state.PoolID,
		AdminStateUp:   boolPointer(true),
		Tags:           tags,
	}).Extract()
	r.record("create", roleL7Policy, resourceID(policy), started, err)
	if err != nil {
		return fmt.Errorf("create L7 policy: %w", err)
	}
	r.state.L7PolicyID = policy.ID
	r.report.Resources.L7PolicyID = policy.ID
	if err := r.saveState(); err != nil {
		return err
	}
	if _, err := r.waitActive(ctx); err != nil {
		return err
	}

	description := r.state.Identity.Description(roleL7Policy) + "; phase=verified"
	started = time.Now()
	_, err = l7policies.Update(ctx, r.clients.loadBalancer, policy.ID, l7policies.UpdateOpts{
		Description: &description,
		Tags:        &tags,
	}).Extract()
	r.record("update", roleL7Policy, policy.ID, started, err)
	if err != nil {
		return fmt.Errorf("update L7 policy: %w", err)
	}
	_, err = r.waitActive(ctx)
	return err
}

func (r *runner) createAndUpdateL7Rule(ctx context.Context) error {
	tags, err := r.state.Identity.Tags(roleL7Rule)
	if err != nil {
		return err
	}
	started := time.Now()
	rule, err := l7policies.CreateRule(
		ctx,
		r.clients.loadBalancer,
		r.state.L7PolicyID,
		l7policies.CreateRuleOpts{
			RuleType:     l7policies.TypePath,
			CompareType:  l7policies.CompareTypeStartWith,
			Value:        "/__gateway_api_openstack_initial_no_match",
			AdminStateUp: boolPointer(true),
			Tags:         tags,
		},
	).Extract()
	r.record("create", roleL7Rule, resourceID(rule), started, err)
	if err != nil {
		return fmt.Errorf("create L7 rule: %w", err)
	}
	r.state.L7RuleID = rule.ID
	r.report.Resources.L7RuleID = rule.ID
	if err := r.saveState(); err != nil {
		return err
	}
	if _, err := r.waitActive(ctx); err != nil {
		return err
	}

	started = time.Now()
	_, err = l7policies.UpdateRule(
		ctx,
		r.clients.loadBalancer,
		r.state.L7PolicyID,
		rule.ID,
		l7policies.UpdateRuleOpts{
			RuleType:    l7policies.TypePath,
			CompareType: l7policies.CompareTypeStartWith,
			Value:       r.config.MatchPath,
			Tags:        &tags,
		},
	).Extract()
	r.record("update", roleL7Rule, rule.ID, started, err)
	if err != nil {
		return fmt.Errorf("update L7 rule: %w", err)
	}
	_, err = r.waitActive(ctx)
	return err
}

func (r *runner) inspectResources(ctx context.Context) error {
	started := time.Now()
	identity := r.state.Identity

	loadBalancer, err := loadbalancers.Get(ctx, r.clients.loadBalancer, r.state.LoadBalancerID).Extract()
	if err == nil && !identity.Matches(loadBalancer.Tags, roleLoadBalancer) {
		err = errors.New("load balancer identity mismatch")
	}
	if err == nil {
		var listener *listeners.Listener
		listener, err = listeners.Get(ctx, r.clients.loadBalancer, r.state.ListenerID).Extract()
		if err == nil && !identity.Matches(listener.Tags, roleListener) {
			err = errors.New("listener identity mismatch")
		}
	}
	if err == nil {
		var pool *pools.Pool
		pool, err = pools.Get(ctx, r.clients.loadBalancer, r.state.PoolID).Extract()
		if err == nil && !identity.Matches(pool.Tags, rolePool) {
			err = errors.New("pool identity mismatch")
		}
	}
	for _, memberID := range r.state.MemberIDs {
		if err != nil {
			break
		}
		var member *pools.Member
		member, err = pools.GetMember(ctx, r.clients.loadBalancer, r.state.PoolID, memberID).Extract()
		if err == nil && !identity.Matches(member.Tags, roleMember) {
			err = fmt.Errorf("member %s identity mismatch", memberID)
		}
	}
	if err == nil {
		var monitor *monitors.Monitor
		monitor, err = monitors.Get(ctx, r.clients.loadBalancer, r.state.MonitorID).Extract()
		if err == nil && !identity.Matches(monitor.Tags, roleMonitor) {
			err = errors.New("health monitor identity mismatch")
		}
	}
	if err == nil {
		var policy *l7policies.L7Policy
		policy, err = l7policies.Get(ctx, r.clients.loadBalancer, r.state.L7PolicyID).Extract()
		if err == nil && !identity.Matches(policy.Tags, roleL7Policy) {
			err = errors.New("L7 policy identity mismatch")
		}
	}
	if err == nil {
		var rule *l7policies.Rule
		rule, err = l7policies.GetRule(
			ctx,
			r.clients.loadBalancer,
			r.state.L7PolicyID,
			r.state.L7RuleID,
		).Extract()
		if err == nil && !identity.Matches(rule.Tags, roleL7Rule) {
			err = errors.New("L7 rule identity mismatch")
		}
	}
	if err == nil && r.state.FloatingIPID != "" {
		var floatingIP *floatingips.FloatingIP
		floatingIP, err = floatingips.Get(ctx, r.clients.network, r.state.FloatingIPID).Extract()
		if err == nil && (!identity.MatchesDescription(floatingIP.Description, roleFloatingIP) ||
			floatingIP.PortID != r.state.VIPPortID) {
			err = errors.New("Floating IP identity mismatch")
		}
	}

	r.record("inspect", "resource-graph", r.state.LoadBalancerID, started, err)
	if err != nil {
		return fmt.Errorf("inspect managed resources: %w", err)
	}
	return nil
}

func (r *runner) checkTraffic(ctx context.Context) error {
	address := r.report.Resources.FloatingIP
	if address == "" {
		address = r.report.Resources.VIPAddress
	}
	target := "http://" + net.JoinHostPort(address, strconv.Itoa(r.config.ListenerPort)) + r.config.RequestPath
	record := &TrafficRecord{
		URL:  target,
		Host: r.config.Host,
	}
	r.report.Traffic = record

	checkCtx, cancel := context.WithTimeout(ctx, r.config.TrafficTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(checkCtx, http.MethodGet, target, nil)
	if err != nil {
		record.Error = err.Error()
		return fmt.Errorf("build traffic request: %w", err)
	}
	if r.config.Host != "" {
		request.Host = r.config.Host
	}

	started := time.Now()
	response, err := http.DefaultClient.Do(request)
	if err == nil {
		defer response.Body.Close()
		record.StatusCode = response.StatusCode
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
		if readErr != nil {
			err = readErr
		} else {
			digest := sha256.Sum256(body)
			record.BodySampleBytes = len(body)
			record.BodySampleSHA256 = fmt.Sprintf("%x", digest)
		}
		if err == nil && (response.StatusCode < 200 || response.StatusCode >= 400) {
			err = fmt.Errorf("unexpected HTTP status %d", response.StatusCode)
		}
	}
	record.Succeeded = err == nil
	if err != nil {
		record.Error = err.Error()
	}
	r.record("request", "backend-traffic", "", started, err)
	if err != nil {
		return fmt.Errorf("traffic check: %w", err)
	}
	return nil
}

func (r *runner) waitActive(ctx context.Context) (*loadbalancers.LoadBalancer, error) {
	return waitLoadBalancerActive(
		ctx,
		r.clients.loadBalancer,
		r.state.LoadBalancerID,
		r.config.OperationTimeout,
		r.config.PollInterval,
	)
}

func (r *runner) saveState() error {
	return SaveState(r.config.StateFile, r.state)
}

func (r *runner) resourceName(role string) string {
	parts := []string{
		"gateway-test-0",
		r.state.Identity.GatewayNamespace,
		r.state.Identity.GatewayName,
		shortValue(r.state.Identity.GatewayUID, 8),
		role,
	}
	return shortValue(sanitizeName(strings.Join(parts, "-")), 200)
}

func sanitizeName(value string) string {
	value = strings.ToLower(value)
	var result strings.Builder
	lastWasDash := false
	for _, character := range value {
		allowed := character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9'
		if allowed {
			result.WriteRune(character)
			lastWasDash = false
			continue
		}
		if !lastWasDash {
			result.WriteByte('-')
			lastWasDash = true
		}
	}
	return strings.Trim(result.String(), "-")
}

func shortValue(value string, length int) string {
	if len(value) <= length {
		return value
	}
	return value[:length]
}

func boolPointer(value bool) *bool {
	return &value
}

func resourceID(resource any) string {
	switch value := resource.(type) {
	case *loadbalancers.LoadBalancer:
		if value != nil {
			return value.ID
		}
	case *listeners.Listener:
		if value != nil {
			return value.ID
		}
	case *pools.Pool:
		if value != nil {
			return value.ID
		}
	case *pools.Member:
		if value != nil {
			return value.ID
		}
	case *monitors.Monitor:
		if value != nil {
			return value.ID
		}
	case *l7policies.L7Policy:
		if value != nil {
			return value.ID
		}
	case *l7policies.Rule:
		if value != nil {
			return value.ID
		}
	case *floatingips.FloatingIP:
		if value != nil {
			return value.ID
		}
	}
	return ""
}
