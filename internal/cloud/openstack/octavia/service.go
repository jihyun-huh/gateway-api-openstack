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

import "github.com/gophercloud/gophercloud/v2"

// Service performs Octavia resource operations with a shared authenticated
// client. Project scoping and ownership validation remain in the callers.
type Service struct {
	client *gophercloud.ServiceClient
}

// NewService creates an Octavia service around the shared authenticated client.
func NewService(client *gophercloud.ServiceClient) *Service {
	return &Service{client: client}
}
