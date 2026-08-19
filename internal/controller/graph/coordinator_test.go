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

package graph

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

func TestGraphCoordinatorSerializesSameGateway(t *testing.T) {
	coordinator := &Coordinator{}
	uid := types.UID("gateway-1")

	releaseFirst, err := Acquire(context.Background(), coordinator, string(uid))
	if err != nil {
		t.Fatalf("acquire first graph lock: %v", err)
	}

	acquiredSecond := make(chan func(), 1)
	errSecond := make(chan error, 1)
	go func() {
		release, err := Acquire(context.Background(), coordinator, string(uid))
		if err != nil {
			errSecond <- err
			return
		}
		acquiredSecond <- release
	}()

	waitForGraphReferences(t, coordinator, uid, 2)
	select {
	case release := <-acquiredSecond:
		release()
		t.Fatal("second caller acquired the same Gateway before release")
	case err := <-errSecond:
		t.Fatalf("acquire second graph lock: %v", err)
	default:
	}

	releaseFirst()
	select {
	case release := <-acquiredSecond:
		release()
	case err := <-errSecond:
		t.Fatalf("acquire second graph lock: %v", err)
	case <-time.After(time.Second):
		t.Fatal("second caller did not acquire the released Gateway")
	}

	waitForGraphReferences(t, coordinator, uid, 0)
}

func TestGraphCoordinatorAllowsDifferentGateways(t *testing.T) {
	coordinator := &Coordinator{}

	releaseFirst, err := Acquire(context.Background(), coordinator, "gateway-1")
	if err != nil {
		t.Fatalf("acquire first Gateway: %v", err)
	}
	defer releaseFirst()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	releaseSecond, err := Acquire(ctx, coordinator, "gateway-2")
	if err != nil {
		t.Fatalf("acquire different Gateway: %v", err)
	}
	releaseSecond()
}

func TestGraphCoordinatorRemovesCanceledWaiter(t *testing.T) {
	coordinator := &Coordinator{}
	uid := types.UID("gateway-1")

	release, err := Acquire(context.Background(), coordinator, string(uid))
	if err != nil {
		t.Fatalf("acquire graph lock: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	waiterResult := make(chan error, 1)
	go func() {
		_, err := Acquire(ctx, coordinator, string(uid))
		waiterResult <- err
	}()

	waitForGraphReferences(t, coordinator, uid, 2)
	cancel()
	select {
	case err := <-waiterResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Acquire() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter did not return")
	}

	waitForGraphReferences(t, coordinator, uid, 1)
	release()
	waitForGraphReferences(t, coordinator, uid, 0)
}

func TestGraphCoordinatorReleaseIsIdempotent(t *testing.T) {
	coordinator := &Coordinator{}
	uid := types.UID("gateway-1")

	release, err := Acquire(context.Background(), coordinator, string(uid))
	if err != nil {
		t.Fatalf("acquire graph lock: %v", err)
	}

	var waitGroup sync.WaitGroup
	for range 10 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			release()
		}()
	}
	waitGroup.Wait()
	waitForGraphReferences(t, coordinator, uid, 0)

	releaseAgain, err := Acquire(context.Background(), coordinator, string(uid))
	if err != nil {
		t.Fatalf("acquire graph lock after repeated release: %v", err)
	}
	releaseAgain()
}

func TestGraphCoordinatorRejectsEmptyGatewayUID(t *testing.T) {
	coordinator := &Coordinator{}

	_, err := Acquire(context.Background(), coordinator, "")
	if !errors.Is(err, errEmptyGatewayUID) {
		t.Fatalf("Acquire() error = %v, want %v", err, errEmptyGatewayUID)
	}
}

func TestAcquireGatewayGraphRequiresCoordinator(t *testing.T) {
	_, err := Acquire(context.Background(), nil, "gateway-1")
	if !errors.Is(err, ErrCoordinatorRequired) {
		t.Fatalf("Acquire() error = %v, want %v", err, ErrCoordinatorRequired)
	}
}

func TestGraphCoordinatorDoesNotRetainCanceledContext(t *testing.T) {
	coordinator := &Coordinator{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Acquire(ctx, coordinator, "gateway-1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire() error = %v, want context.Canceled", err)
	}
	if got := graphReferenceCount(coordinator, types.UID("gateway-1")); got != 0 {
		t.Fatalf("reference count = %d, want 0", got)
	}
}

func waitForGraphReferences(t *testing.T, coordinator *Coordinator, uid types.UID, want int) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for {
		got, exists := graphReferenceState(coordinator, uid)
		if (want == 0 && !exists) || (want > 0 && exists && got == want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("reference count did not become %d", want)
		}
		time.Sleep(time.Millisecond)
	}
}

func graphReferenceCount(coordinator *Coordinator, uid types.UID) int {
	count, _ := graphReferenceState(coordinator, uid)
	return count
}

func graphReferenceState(coordinator *Coordinator, uid types.UID) (int, bool) {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()

	if entry := coordinator.entries[uid]; entry != nil {
		return entry.refs, true
	}
	return 0, false
}
