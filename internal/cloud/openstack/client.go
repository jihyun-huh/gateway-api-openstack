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
	"crypto/tls"
	"fmt"
	"strconv"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	gopheropenstack "github.com/gophercloud/gophercloud/v2/openstack"
	openstackconfig "github.com/gophercloud/gophercloud/v2/openstack/config"
	"github.com/gophercloud/gophercloud/v2/openstack/config/clouds"
	tokensv2 "github.com/gophercloud/gophercloud/v2/openstack/identity/v2/tokens"
	tokensv3 "github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
)

// ClientConfig selects authentication and service endpoints. Credentials are
// read at startup and are never copied into Kubernetes status or logs.
type ClientConfig struct {
	Region         string
	Microversion   string
	CloudsYAMLPath string
	CloudName      string
	AllowInsecure  bool
}

// ServiceClients contains the Gophercloud clients shared by the controller
// implementation and the retained Phase 0 capability probe.
type ServiceClients struct {
	LoadBalancer *gophercloud.ServiceClient
	Network      *gophercloud.ServiceClient
	ProjectID    string
}

// NewServiceClients authenticates from a selected clouds.yaml entry when a
// path is supplied, otherwise from standard OS_* environment variables.
func NewServiceClients(ctx context.Context, cfg ClientConfig) (ServiceClients, error) {
	if err := ValidateMicroversion(cfg.Microversion); err != nil {
		return ServiceClients{}, err
	}
	authOptions, endpointOptions, tlsConfig, err := authenticationOptions(cfg)
	if err != nil {
		return ServiceClients{}, err
	}
	if tlsConfig != nil && tlsConfig.InsecureSkipVerify && !cfg.AllowInsecure {
		return ServiceClients{}, fmt.Errorf("clouds.yaml disables TLS verification; pass --insecure only for an explicitly approved test cloud")
	}
	if cfg.AllowInsecure {
		if tlsConfig == nil {
			tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		} else {
			tlsConfig = tlsConfig.Clone()
		}
		tlsConfig.InsecureSkipVerify = true //nolint:gosec // Explicit test-cloud-only option.
	}
	// An already-issued, unscoped token is a Keystone passthrough credential:
	// Gophercloud cannot obtain a replacement token without separate scope and
	// credentials. Enabling reauthentication for that form is rejected before
	// the first request. Scoped token authentication and renewable credential
	// forms retain automatic reauthentication.
	authOptions.AllowReauth = allowReauthentication(authOptions)
	var provider *gophercloud.ProviderClient
	if tlsConfig != nil {
		provider, err = openstackconfig.NewProviderClient(ctx, authOptions, openstackconfig.WithTLSConfig(tlsConfig))
	} else {
		provider, err = openstackconfig.NewProviderClient(ctx, authOptions)
	}
	if err != nil {
		return ServiceClients{}, fmt.Errorf("authenticate to OpenStack: %w", err)
	}
	projectID, err := authenticatedProjectID(provider, authOptions)
	if err != nil {
		return ServiceClients{}, err
	}

	loadBalancerClient, err := gopheropenstack.NewLoadBalancerV2(provider, endpointOptions)
	if err != nil {
		return ServiceClients{}, fmt.Errorf("create Octavia client: %w", err)
	}
	loadBalancerClient.Microversion = cfg.Microversion

	networkClient, err := gopheropenstack.NewNetworkV2(provider, endpointOptions)
	if err != nil {
		return ServiceClients{}, fmt.Errorf("create Neutron client: %w", err)
	}

	return ServiceClients{
		LoadBalancer: loadBalancerClient,
		Network:      networkClient,
		ProjectID:    projectID,
	}, nil
}

func authenticatedProjectID(provider *gophercloud.ProviderClient, authOptions gophercloud.AuthOptions) (string, error) {
	switch result := provider.GetAuthResult().(type) {
	case tokensv3.CreateResult:
		project, err := result.ExtractProject()
		if err != nil {
			return "", fmt.Errorf("extract project from Keystone token: %w", err)
		}
		if project != nil && project.ID != "" {
			return project.ID, nil
		}
	case tokensv3.GetResult:
		project, err := result.ExtractProject()
		if err != nil {
			return "", fmt.Errorf("extract project from validated Keystone token: %w", err)
		}
		if project != nil && project.ID != "" {
			return project.ID, nil
		}
	case tokensv2.CreateResult:
		token, err := result.ExtractToken()
		if err != nil {
			return "", fmt.Errorf("extract tenant from Keystone token: %w", err)
		}
		if token.Tenant.ID != "" {
			return token.Tenant.ID, nil
		}
	}
	if authOptions.TenantID != "" {
		return authOptions.TenantID, nil
	}
	return "", fmt.Errorf("authentication must be scoped to an OpenStack project so managed resources have a stable project identity")
}

func allowReauthentication(authOptions gophercloud.AuthOptions) bool {
	if authOptions.TokenID == "" {
		return true
	}
	return authOptions.Scope != nil && *authOptions.Scope != (gophercloud.AuthScope{})
}

// ValidateMicroversion enforces the tag support required by the ownership
// contract. It parses major and minor components as integers, so 2.10 is not
// mistaken for a floating-point value smaller than 2.5.
func ValidateMicroversion(value string) error {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return fmt.Errorf("octavia microversion %q must have major.minor form", value)
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	if majorErr != nil || minorErr != nil || major < 0 || minor < 0 {
		return fmt.Errorf("octavia microversion %q must contain non-negative integers", value)
	}
	if major < 2 || major == 2 && minor < 5 {
		return fmt.Errorf("octavia microversion %q is unsupported; 2.5 or newer is required for identity tags", value)
	}
	return nil
}

func authenticationOptions(cfg ClientConfig) (gophercloud.AuthOptions, gophercloud.EndpointOpts, *tls.Config, error) {
	if cfg.CloudsYAMLPath == "" {
		authOptions, err := gopheropenstack.AuthOptionsFromEnv()
		if err != nil {
			return gophercloud.AuthOptions{}, gophercloud.EndpointOpts{}, nil, fmt.Errorf("read OpenStack authentication environment: %w", err)
		}
		if strings.TrimSpace(authOptions.IdentityEndpoint) == "" {
			return gophercloud.AuthOptions{}, gophercloud.EndpointOpts{}, nil, fmt.Errorf("environment variable OS_AUTH_URL is required when --clouds-yaml is not set")
		}
		return authOptions, gophercloud.EndpointOpts{Region: cfg.Region}, nil, nil
	}
	if strings.TrimSpace(cfg.CloudName) == "" {
		return gophercloud.AuthOptions{}, gophercloud.EndpointOpts{}, nil, fmt.Errorf("--openstack-cloud is required with --clouds-yaml")
	}
	options := []clouds.ParseOption{
		clouds.WithCloudName(cfg.CloudName),
		clouds.WithLocations(cfg.CloudsYAMLPath),
	}
	if cfg.Region != "" {
		options = append(options, clouds.WithRegion(cfg.Region))
	}
	authOptions, endpointOptions, tlsConfig, err := clouds.Parse(options...)
	if err != nil {
		return gophercloud.AuthOptions{}, gophercloud.EndpointOpts{}, nil, fmt.Errorf("parse clouds.yaml: %w", err)
	}
	return authOptions, endpointOptions, tlsConfig, nil
}
