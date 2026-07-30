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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const stateVersion = 1

// State is persisted after every successful create so cleanup can resume.
type State struct {
	Version             int       `json:"version"`
	CreatedAt           time.Time `json:"createdAt"`
	Region              string    `json:"region,omitempty"`
	OctaviaMicroversion string    `json:"octaviaMicroversion"`
	Identity            Identity  `json:"identity"`
	LoadBalancerID      string    `json:"loadBalancerID,omitempty"`
	VIPPortID           string    `json:"vipPortID,omitempty"`
	FloatingIPID        string    `json:"floatingIPID,omitempty"`
	ListenerID          string    `json:"listenerID,omitempty"`
	PoolID              string    `json:"poolID,omitempty"`
	MemberIDs           []string  `json:"memberIDs,omitempty"`
	MonitorID           string    `json:"monitorID,omitempty"`
	L7PolicyID          string    `json:"l7PolicyID,omitempty"`
	L7RuleID            string    `json:"l7RuleID,omitempty"`
}

// NewState creates empty state for one run.
func NewState(cfg Config) State {
	return State{
		Version:             stateVersion,
		CreatedAt:           time.Now().UTC(),
		Region:              cfg.Region,
		OctaviaMicroversion: cfg.OctaviaMicroversion,
		Identity:            NewIdentity(cfg),
	}
}

// CreateState claims a new state path without overwriting an earlier run.
func CreateState(path string, state State) error {
	if path == "" {
		return errors.New("state path must not be empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}

	data, err := encodeState(state)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf(
			"state file %s already exists; clean up the previous run before starting another",
			path,
		)
	}
	if err != nil {
		return fmt.Errorf("create state file: %w", err)
	}

	complete := false
	defer func() {
		file.Close()
		if !complete {
			os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write initial state: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync initial state: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close initial state: %w", err)
	}
	complete = true
	return nil
}

// SaveState atomically writes state with user-only permissions.
func SaveState(path string, state State) error {
	if path == "" {
		return errors.New("state path must not be empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}

	data, err := encodeState(state)
	if err != nil {
		return err
	}

	temp, err := os.CreateTemp(filepath.Dir(path), ".probe-state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary state: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)

	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("protect temporary state: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write temporary state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync temporary state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary state: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	return nil
}

func encodeState(state State) ([]byte, error) {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode state: %w", err)
	}
	return append(data, '\n'), nil
}

// LoadState reads and validates persisted cleanup state.
func LoadState(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, fmt.Errorf("read state: %w", err)
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("decode state: %w", err)
	}
	if state.Version != stateVersion {
		return State{}, fmt.Errorf("unsupported state version %d", state.Version)
	}
	if err := state.Identity.Validate(); err != nil {
		return State{}, fmt.Errorf("invalid state identity: %w", err)
	}
	return state, nil
}
