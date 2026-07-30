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
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// for test: Report is credential free record responce of one probe execution.
type Report struct {
	SchemaVersion    int               `json:"schemaVersion"`
	StartedAt        time.Time         `json:"startedAt"`
	CompletedAt      time.Time         `json:"completedAt,omitempty"`
	Outcome          string            `json:"outcome"`
	Error            string            `json:"error,omitempty"`
	Environment      ReportEnvironment `json:"environment"`
	Providers        []ProviderRecord  `json:"providers,omitempty"`
	Resources        ResourceRecord    `json:"resources"`
	Steps            []StepRecord      `json:"steps"`
	Traffic          *TrafficRecord    `json:"traffic,omitempty"`
	CleanupAttempted bool              `json:"cleanupAttempted"`
	CleanupSucceeded bool              `json:"cleanupSucceeded"`
}

// for test: ReportEnvironment contains only explicit, none secret test parameters.
type ReportEnvironment struct {
	Region              string   `json:"region,omitempty"`
	Provider            string   `json:"provider"`
	OctaviaMicroversion string   `json:"octaviaMicroversion"`
	VIPSubnetID         string   `json:"vipSubnetID"`
	ExternalNetworkID   string   `json:"externalNetworkID,omitempty"`
	MemberAddresses     []string `json:"memberAddresses"`
	MemberPort          int      `json:"memberPort"`
	MemberSubnetID      string   `json:"memberSubnetID,omitempty"`
	ListenerPort        int      `json:"listenerPort"`
	Identity            Identity `json:"identity"`
}

// for test: ProviderRecord records the driver returned by the Octavia API
type ProviderRecord struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// ResourceRecord records the created IDs and addresses.
type ResourceRecord struct {
	LoadBalancerID string   `json:"loadBalancerID,omitempty"`
	VIPAddress     string   `json:"vipAddress,omitempty"`
	FloatingIPID   string   `json:"floatingIPID,omitempty"`
	FloatingIP     string   `json:"floatingIP,omitempty"`
	ListenerID     string   `json:"listenerID,omitempty"`
	PoolID         string   `json:"poolID,omitempty"`
	MemberIDs      []string `json:"memberIDs,omitempty"`
	MonitorID      string   `json:"monitorID,omitempty"`
	L7PolicyID     string   `json:"l7PolicyID,omitempty"`
	L7RuleID       string   `json:"l7RuleID,omitempty"`
}

// StepRecord describes one API operation without copying an API response.
type StepRecord struct {
	Operation  string        `json:"operation"`
	Resource   string        `json:"resource"`
	ResourceID string        `json:"resourceID,omitempty"`
	Duration   time.Duration `json:"duration"`
	Succeeded  bool          `json:"succeeded"`
	Error      string        `json:"error,omitempty"`
}

// TrafficRecord records the final client-to-backend request.
type TrafficRecord struct {
	URL              string `json:"url"`
	Host             string `json:"host,omitempty"`
	StatusCode       int    `json:"statusCode,omitempty"`
	BodySampleBytes  int    `json:"bodySampleBytes,omitempty"`
	BodySampleSHA256 string `json:"bodySampleSHA256,omitempty"`
	Succeeded        bool   `json:"succeeded"`
	Error            string `json:"error,omitempty"`
}

func newReport(cfg Config) *Report {
	return &Report{
		SchemaVersion: 1,
		StartedAt:     time.Now().UTC(),
		Outcome:       "running",
		Environment: ReportEnvironment{
			Region:              cfg.Region,
			Provider:            cfg.Provider,
			OctaviaMicroversion: cfg.OctaviaMicroversion,
			VIPSubnetID:         cfg.VIPSubnetID,
			ExternalNetworkID:   cfg.ExternalNetworkID,
			MemberAddresses:     append([]string(nil), cfg.MemberAddresses...),
			MemberPort:          cfg.MemberPort,
			MemberSubnetID:      cfg.MemberSubnetID,
			ListenerPort:        cfg.ListenerPort,
			Identity:            NewIdentity(cfg),
		},
	}
}

// SaveReport writes a formatted report. Reports may identify a test project,
// so operators must review them before publishing.
func SaveReport(path string, report *Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create report directory: %w", err)
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}
