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
	"crypto/tls"
	"fmt"
	"net/http"
	"os"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
)

type serviceClients struct {
	loadBalancer *gophercloud.ServiceClient
	network      *gophercloud.ServiceClient
}

func newServiceClients(ctx context.Context, region, microversion string, allowInsecure bool) (serviceClients, error) {
	authOptions, err := openstack.AuthOptionsFromEnv()
	if err != nil {
		return serviceClients{}, fmt.Errorf("read OpenStack authentication environment: %w", err)
	}

	provider, err := openstack.NewClient(authOptions.IdentityEndpoint)
	if err != nil {
		return serviceClients{}, fmt.Errorf("create OpenStack provider client: %w", err)
	}
	if allowInsecure {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true, //nolint:gosec // Explicit Phase 0 test-cloud option.
		}
		provider.HTTPClient.Transport = transport
	}
	if err := openstack.Authenticate(ctx, provider, authOptions); err != nil {
		return serviceClients{}, fmt.Errorf("authenticate to OpenStack: %w", err)
	}

	endpointOptions := gophercloud.EndpointOpts{Region: region}
	loadBalancerClient, err := openstack.NewLoadBalancerV2(provider, endpointOptions)
	if err != nil {
		return serviceClients{}, fmt.Errorf("create Octavia client: %w", err)
	}
	loadBalancerClient.Microversion = microversion

	networkClient, err := openstack.NewNetworkV2(provider, endpointOptions)
	if err != nil {
		return serviceClients{}, fmt.Errorf("create Neutron client: %w", err)
	}

	return serviceClients{
		loadBalancer: loadBalancerClient,
		network:      networkClient,
	}, nil
}

func requireAuthenticationEnvironment() error {
	for _, name := range []string{"OS_AUTH_URL"} {
		if os.Getenv(name) == "" {
			return fmt.Errorf("%s is not set yet, source an OpenStack RC file or export application credential variables", name)
		}
	}
	return nil
}
