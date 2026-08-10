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

	cloudopenstack "github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack"
)

// Phase 0 remains a standalone executable, but authentication and service
// client construction are now production code shared with the controller.
type serviceClients = cloudopenstack.ServiceClients

func newServiceClients(ctx context.Context, region, microversion string, allowInsecure bool) (serviceClients, error) {
	return cloudopenstack.NewServiceClients(ctx, cloudopenstack.ClientConfig{
		Region:        region,
		Microversion:  microversion,
		AllowInsecure: allowInsecure,
		APIQPS:        cloudopenstack.DefaultAPIQPS,
		APIBurst:      cloudopenstack.DefaultAPIBurst,
	})
}
