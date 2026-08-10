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
	"context"
	"errors"
	"sync"

	"k8s.io/apimachinery/pkg/types"
)

var (
	errEmptyGatewayUID          = errors.New("gateway UID is empty")
	errGraphCoordinatorRequired = errors.New("Gateway graph coordinator is required")
)

// GraphCoordinator serializes changes to the OpenStack graph owned by a
// Gateway. Its zero value is ready for use.
type GraphCoordinator struct {
	mu      sync.Mutex
	entries map[types.UID]*graphCoordinatorEntry
}

type graphCoordinatorEntry struct {
	ready chan struct{}
	refs  int
}

// Acquire waits until the caller owns the graph for gatewayUID. The returned
// function releases that ownership and is safe to call more than once.
func (c *GraphCoordinator) Acquire(ctx context.Context, gatewayUID types.UID) (func(), error) {
	if gatewayUID == "" {
		return nil, errEmptyGatewayUID
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	entry := c.retain(gatewayUID)
	select {
	case <-ctx.Done():
		c.releaseReference(gatewayUID, entry)
		return nil, ctx.Err()
	case <-entry.ready:
		if err := ctx.Err(); err != nil {
			entry.ready <- struct{}{}
			c.releaseReference(gatewayUID, entry)
			return nil, err
		}
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			entry.ready <- struct{}{}
			c.releaseReference(gatewayUID, entry)
		})
	}, nil
}

func acquireGatewayGraph(ctx context.Context, coordinator *GraphCoordinator, gatewayUID string) (func(), error) {
	if coordinator == nil {
		return nil, errGraphCoordinatorRequired
	}
	return coordinator.Acquire(ctx, types.UID(gatewayUID))
}

func (c *GraphCoordinator) retain(gatewayUID types.UID) *graphCoordinatorEntry {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.entries == nil {
		c.entries = make(map[types.UID]*graphCoordinatorEntry)
	}
	entry, ok := c.entries[gatewayUID]
	if !ok {
		entry = &graphCoordinatorEntry{ready: make(chan struct{}, 1)}
		entry.ready <- struct{}{}
		c.entries[gatewayUID] = entry
	}
	entry.refs++
	return entry
}

func (c *GraphCoordinator) releaseReference(gatewayUID types.UID, entry *graphCoordinatorEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry.refs--
	if entry.refs == 0 && c.entries[gatewayUID] == entry {
		delete(c.entries, gatewayUID)
	}
}
