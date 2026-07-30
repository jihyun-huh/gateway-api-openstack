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
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/l7policies"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/monitors"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
)

// Cleanup resumes deletion using a state file. It refuses to delete any
// resource that does not carry the complete expected identity.
func Cleanup(ctx context.Context, cfg CleanupConfig) error {
	state, err := LoadState(cfg.StateFile)
	if err != nil {
		return err
	}
	if err := requireAuthenticationEnvironment(); err != nil {
		return err
	}

	region := cfg.Region
	if region == "" {
		region = state.Region
	}
	clients, err := newServiceClients(
		ctx,
		region,
		state.OctaviaMicroversion,
		cfg.AllowInsecureOpenStack,
	)
	if err != nil {
		return err
	}
	return cleanupWithClients(
		ctx,
		clients,
		state,
		cfg.StateFile,
		cfg.OperationTimeout,
		cfg.PollInterval,
	)
}

func cleanupWithClients(
	ctx context.Context,
	clients serviceClients,
	state State,
	stateFile string,
	operationTimeout time.Duration,
	pollInterval time.Duration,
) error {
	timeout := operationTimeout
	interval := pollInterval
	identity := state.Identity

	loadBalancerExists := false
	if state.LoadBalancerID != "" {
		loadBalancer, err := loadbalancers.Get(ctx, clients.loadBalancer, state.LoadBalancerID).Extract()
		switch {
		case err == nil:
			if !identity.Matches(loadBalancer.Tags, roleLoadBalancer) {
				return errors.New("refusing cleanup: load balancer identity does not match state")
			}
			loadBalancerExists = true
		case isNotFound(err):
		default:
			return fmt.Errorf("inspect load balancer before cleanup: %w", err)
		}
	}

	if state.FloatingIPID != "" {
		floatingIP, err := floatingips.Get(ctx, clients.network, state.FloatingIPID).Extract()
		switch {
		case err == nil:
			if !identity.MatchesDescription(floatingIP.Description, roleFloatingIP) ||
				floatingIP.PortID != state.VIPPortID {
				return errors.New("refusing cleanup: Floating IP identity does not match state")
			}
			if err := floatingips.Delete(ctx, clients.network, state.FloatingIPID).ExtractErr(); err != nil {
				return fmt.Errorf("delete Floating IP: %w", err)
			}
		case isNotFound(err):
		default:
			return fmt.Errorf("inspect Floating IP before cleanup: %w", err)
		}
	}

	if loadBalancerExists {
		if err := deleteL7Rule(ctx, clients, state, timeout, interval); err != nil {
			return err
		}
		if err := deleteL7Policy(ctx, clients, state, timeout, interval); err != nil {
			return err
		}
		if err := deleteMonitor(ctx, clients, state, timeout, interval); err != nil {
			return err
		}
		if err := deleteMembers(ctx, clients, state, timeout, interval); err != nil {
			return err
		}
		if err := deletePool(ctx, clients, state, timeout, interval); err != nil {
			return err
		}
		if err := deleteListener(ctx, clients, state, timeout, interval); err != nil {
			return err
		}
		if err := loadbalancers.Delete(
			ctx,
			clients.loadBalancer,
			state.LoadBalancerID,
			loadbalancers.DeleteOpts{Cascade: false},
		).ExtractErr(); err != nil {
			return fmt.Errorf("delete load balancer: %w", err)
		}
		if err := waitLoadBalancerDeleted(
			ctx,
			clients.loadBalancer,
			state.LoadBalancerID,
			timeout,
			interval,
		); err != nil {
			return err
		}
	}

	if err := os.Remove(stateFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove completed state file: %w", err)
	}
	return nil
}

func deleteL7Rule(
	ctx context.Context,
	clients serviceClients,
	state State,
	timeout, interval time.Duration,
) error {
	if state.L7PolicyID == "" || state.L7RuleID == "" {
		return nil
	}
	rule, err := l7policies.GetRule(
		ctx,
		clients.loadBalancer,
		state.L7PolicyID,
		state.L7RuleID,
	).Extract()
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect L7 rule before cleanup: %w", err)
	}
	if !state.Identity.Matches(rule.Tags, roleL7Rule) {
		return errors.New("refusing cleanup: L7 rule identity does not match state")
	}
	if err := l7policies.DeleteRule(
		ctx,
		clients.loadBalancer,
		state.L7PolicyID,
		state.L7RuleID,
	).ExtractErr(); err != nil {
		return fmt.Errorf("delete L7 rule: %w", err)
	}
	return waitAfterChildDelete(ctx, clients, state.LoadBalancerID, timeout, interval)
}

func deleteL7Policy(
	ctx context.Context,
	clients serviceClients,
	state State,
	timeout, interval time.Duration,
) error {
	if state.L7PolicyID == "" {
		return nil
	}
	policy, err := l7policies.Get(ctx, clients.loadBalancer, state.L7PolicyID).Extract()
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect L7 policy before cleanup: %w", err)
	}
	if !state.Identity.Matches(policy.Tags, roleL7Policy) {
		return errors.New("refusing cleanup: L7 policy identity does not match state")
	}
	if err := l7policies.Delete(ctx, clients.loadBalancer, state.L7PolicyID).ExtractErr(); err != nil {
		return fmt.Errorf("delete L7 policy: %w", err)
	}
	return waitAfterChildDelete(ctx, clients, state.LoadBalancerID, timeout, interval)
}

func deleteMonitor(
	ctx context.Context,
	clients serviceClients,
	state State,
	timeout, interval time.Duration,
) error {
	if state.MonitorID == "" {
		return nil
	}
	monitor, err := monitors.Get(ctx, clients.loadBalancer, state.MonitorID).Extract()
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect health monitor before cleanup: %w", err)
	}
	if !state.Identity.Matches(monitor.Tags, roleMonitor) {
		return errors.New("refusing cleanup: health monitor identity does not match state")
	}
	if err := monitors.Delete(ctx, clients.loadBalancer, state.MonitorID).ExtractErr(); err != nil {
		return fmt.Errorf("delete health monitor: %w", err)
	}
	return waitAfterChildDelete(ctx, clients, state.LoadBalancerID, timeout, interval)
}

func deleteMembers(
	ctx context.Context,
	clients serviceClients,
	state State,
	timeout, interval time.Duration,
) error {
	for _, memberID := range state.MemberIDs {
		member, err := pools.GetMember(ctx, clients.loadBalancer, state.PoolID, memberID).Extract()
		if isNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect member %s before cleanup: %w", memberID, err)
		}
		if !state.Identity.Matches(member.Tags, roleMember) {
			return fmt.Errorf("refusing cleanup: member %s identity does not match state", memberID)
		}
		if err := pools.DeleteMember(
			ctx,
			clients.loadBalancer,
			state.PoolID,
			memberID,
		).ExtractErr(); err != nil {
			return fmt.Errorf("delete member %s: %w", memberID, err)
		}
		if err := waitAfterChildDelete(
			ctx,
			clients,
			state.LoadBalancerID,
			timeout,
			interval,
		); err != nil {
			return err
		}
	}
	return nil
}

func deletePool(
	ctx context.Context,
	clients serviceClients,
	state State,
	timeout, interval time.Duration,
) error {
	if state.PoolID == "" {
		return nil
	}
	pool, err := pools.Get(ctx, clients.loadBalancer, state.PoolID).Extract()
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect pool before cleanup: %w", err)
	}
	if !state.Identity.Matches(pool.Tags, rolePool) {
		return errors.New("refusing cleanup: pool identity does not match state")
	}
	if err := pools.Delete(ctx, clients.loadBalancer, state.PoolID).ExtractErr(); err != nil {
		return fmt.Errorf("delete pool: %w", err)
	}
	return waitAfterChildDelete(ctx, clients, state.LoadBalancerID, timeout, interval)
}

func deleteListener(
	ctx context.Context,
	clients serviceClients,
	state State,
	timeout, interval time.Duration,
) error {
	if state.ListenerID == "" {
		return nil
	}
	listener, err := listeners.Get(ctx, clients.loadBalancer, state.ListenerID).Extract()
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect listener before cleanup: %w", err)
	}
	if !state.Identity.Matches(listener.Tags, roleListener) {
		return errors.New("refusing cleanup: listener identity does not match state")
	}
	if err := listeners.Delete(ctx, clients.loadBalancer, state.ListenerID).ExtractErr(); err != nil {
		return fmt.Errorf("delete listener: %w", err)
	}
	return waitAfterChildDelete(ctx, clients, state.LoadBalancerID, timeout, interval)
}

func waitAfterChildDelete(
	ctx context.Context,
	clients serviceClients,
	loadBalancerID string,
	timeout, interval time.Duration,
) error {
	_, err := waitLoadBalancerActive(
		ctx,
		clients.loadBalancer,
		loadBalancerID,
		timeout,
		interval,
	)
	return err
}

func isNotFound(err error) bool {
	return gophercloud.ResponseCodeIs(err, http.StatusNotFound)
}
