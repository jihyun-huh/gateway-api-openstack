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

package controller

import (
	"strings"
	"time"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud"
)

const (
	minimumProviderProgressRequeue = time.Second
	defaultProviderProgressRequeue = 5 * time.Second
	maximumProviderProgressRequeue = 30 * time.Second
)

func providerProgressRequeueAfter(outcome cloud.Outcome) time.Duration {
	switch {
	case outcome.RequeueAfter <= 0:
		return defaultProviderProgressRequeue
	case outcome.RequeueAfter < minimumProviderProgressRequeue:
		return minimumProviderProgressRequeue
	case outcome.RequeueAfter > maximumProviderProgressRequeue:
		return maximumProviderProgressRequeue
	default:
		return outcome.RequeueAfter
	}
}

func providerProgressMessage(outcome cloud.Outcome, fallback string) string {
	if message := strings.TrimSpace(outcome.Message); message != "" {
		return message
	}
	return fallback
}
