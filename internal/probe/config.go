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
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultStateFile  = "_artifacts/phase-0/probe-state.json"
	defaultReportFile = "_artifacts/phase-0/probe-report.json"
)

// Config contains non-secret inputs for one capability-probe run.
type Config struct {
	Region                 string
	VIPSubnetID            string
	ExternalNetworkID      string
	Provider               string
	OctaviaMicroversion    string
	MemberAddresses        []string
	MemberPort             int
	MemberSubnetID         string
	ListenerPort           int
	HealthPath             string
	MatchPath              string
	RequestPath            string
	Host                   string
	ClusterID              string
	GatewayNamespace       string
	GatewayName            string
	GatewayUID             string
	StateFile              string
	ReportFile             string
	OperationTimeout       time.Duration
	PollInterval           time.Duration
	TrafficTimeout         time.Duration
	KeepResources          bool
	SkipTrafficCheck       bool
	AllowInsecureOpenStack bool
}

// CleanupConfig contains the inputs needed to resume identity-safe cleanup.
type CleanupConfig struct {
	Region                 string
	StateFile              string
	OperationTimeout       time.Duration
	PollInterval           time.Duration
	AllowInsecureOpenStack bool
}

// ParseRunFlags parses flags without reading or logging credential values.
func ParseRunFlags(args []string, getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	var cfg Config
	var memberAddresses string

	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&cfg.Region, "region", getenv("OS_REGION_NAME"), "OpenStack region")
	flags.StringVar(&cfg.VIPSubnetID, "vip-subnet-id", getenv("GATEWAY_OPENSTACK_VIP_SUBNET_ID"), "Octavia VIP subnet UUID")
	flags.StringVar(&cfg.ExternalNetworkID, "external-network-id", getenv("GATEWAY_OPENSTACK_EXTERNAL_NETWORK_ID"), "Neutron external network UUID; empty disables Floating IP creation")
	flags.StringVar(&cfg.Provider, "provider", envOr(getenv, "GATEWAY_OPENSTACK_PROVIDER", "amphora"), "Octavia provider name")
	flags.StringVar(&cfg.OctaviaMicroversion, "octavia-microversion", envOr(getenv, "GATEWAY_OPENSTACK_OCTAVIA_MICROVERSION", "2.5"), "Octavia API microversion")
	flags.StringVar(&memberAddresses, "member-addresses", getenv("GATEWAY_OPENSTACK_MEMBER_ADDRESSES"), "comma-separated worker or Pod IP addresses")
	flags.IntVar(&cfg.MemberPort, "member-port", envInt(getenv, "GATEWAY_OPENSTACK_MEMBER_PORT", 0), "backend NodePort or application port")
	flags.StringVar(&cfg.MemberSubnetID, "member-subnet-id", getenv("GATEWAY_OPENSTACK_MEMBER_SUBNET_ID"), "member subnet UUID when required by the provider")
	flags.IntVar(&cfg.ListenerPort, "listener-port", envInt(getenv, "GATEWAY_OPENSTACK_LISTENER_PORT", 80), "HTTP listener port")
	flags.StringVar(&cfg.HealthPath, "health-path", envOr(getenv, "GATEWAY_OPENSTACK_HEALTH_PATH", "/healthz"), "HTTP health-monitor path")
	flags.StringVar(&cfg.MatchPath, "match-path", envOr(getenv, "GATEWAY_OPENSTACK_MATCH_PATH", "/"), "L7 path-prefix match")
	flags.StringVar(&cfg.RequestPath, "request-path", envOr(getenv, "GATEWAY_OPENSTACK_REQUEST_PATH", "/"), "path used for the traffic check")
	flags.StringVar(&cfg.Host, "host", getenv("GATEWAY_OPENSTACK_HOST"), "optional HTTP Host header")
	flags.StringVar(&cfg.ClusterID, "cluster-id", getenv("GATEWAY_OPENSTACK_CLUSTER_ID"), "stable, non-secret cluster identifier")
	flags.StringVar(&cfg.GatewayNamespace, "gateway-namespace", envOr(getenv, "GATEWAY_OPENSTACK_GATEWAY_NAMESPACE", "phase-0"), "synthetic Gateway namespace")
	flags.StringVar(&cfg.GatewayName, "gateway-name", envOr(getenv, "GATEWAY_OPENSTACK_GATEWAY_NAME", "octavia-probe"), "synthetic Gateway name")
	flags.StringVar(&cfg.GatewayUID, "gateway-uid", getenv("GATEWAY_OPENSTACK_GATEWAY_UID"), "synthetic immutable Gateway UID")
	flags.StringVar(&cfg.StateFile, "state-file", envOr(getenv, "GATEWAY_OPENSTACK_STATE_FILE", defaultStateFile), "resumable cleanup state file")
	flags.StringVar(&cfg.ReportFile, "report-file", envOr(getenv, "GATEWAY_OPENSTACK_REPORT_FILE", defaultReportFile), "local JSON report file; review before publishing")
	flags.DurationVar(&cfg.OperationTimeout, "operation-timeout", envDuration(getenv, "GATEWAY_OPENSTACK_OPERATION_TIMEOUT", 20*time.Minute), "maximum wait for an Octavia operation")
	flags.DurationVar(&cfg.PollInterval, "poll-interval", envDuration(getenv, "GATEWAY_OPENSTACK_POLL_INTERVAL", 5*time.Second), "Octavia status polling interval")
	flags.DurationVar(&cfg.TrafficTimeout, "traffic-timeout", envDuration(getenv, "GATEWAY_OPENSTACK_TRAFFIC_TIMEOUT", 30*time.Second), "HTTP traffic-check timeout")
	flags.BoolVar(&cfg.KeepResources, "keep-resources", envBool(getenv, "GATEWAY_OPENSTACK_KEEP_RESOURCES", false), "leave resources for inspection and use the cleanup subcommand later")
	flags.BoolVar(&cfg.SkipTrafficCheck, "skip-traffic-check", envBool(getenv, "GATEWAY_OPENSTACK_SKIP_TRAFFIC_CHECK", false), "skip the final HTTP request")
	flags.BoolVar(&cfg.AllowInsecureOpenStack, "insecure", envBool(getenv, "OS_INSECURE", false), "disable OpenStack TLS verification; test clouds only")

	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if flags.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}

	cfg.MemberAddresses = splitNonEmpty(memberAddresses)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// ParseCleanupFlags parses flags for a resumed cleanup.
func ParseCleanupFlags(args []string, getenv func(string) string) (CleanupConfig, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	var cfg CleanupConfig
	flags := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&cfg.Region, "region", getenv("OS_REGION_NAME"), "OpenStack region")
	flags.StringVar(&cfg.StateFile, "state-file", envOr(getenv, "GATEWAY_OPENSTACK_STATE_FILE", defaultStateFile), "probe state file")
	flags.DurationVar(&cfg.OperationTimeout, "operation-timeout", envDuration(getenv, "GATEWAY_OPENSTACK_OPERATION_TIMEOUT", 20*time.Minute), "maximum wait for an Octavia operation")
	flags.DurationVar(&cfg.PollInterval, "poll-interval", envDuration(getenv, "GATEWAY_OPENSTACK_POLL_INTERVAL", 5*time.Second), "Octavia status polling interval")
	flags.BoolVar(&cfg.AllowInsecureOpenStack, "insecure", envBool(getenv, "OS_INSECURE", false), "disable OpenStack TLS verification; test clouds only")

	if err := flags.Parse(args); err != nil {
		return CleanupConfig{}, err
	}
	if flags.NArg() != 0 {
		return CleanupConfig{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if cfg.StateFile == "" {
		return CleanupConfig{}, errors.New("--state-file must not be empty")
	}
	if cfg.OperationTimeout <= 0 {
		return CleanupConfig{}, errors.New("--operation-timeout must be positive")
	}
	if cfg.PollInterval <= 0 {
		return CleanupConfig{}, errors.New("--poll-interval must be positive")
	}
	return cfg, nil
}

// Validate checks inputs that would otherwise fail after cloud resources exist.
func (cfg Config) Validate() error {
	var missing []string
	for name, value := range map[string]string{
		"--vip-subnet-id":     cfg.VIPSubnetID,
		"--member-addresses":  strings.Join(cfg.MemberAddresses, ","),
		"--cluster-id":        cfg.ClusterID,
		"--gateway-namespace": cfg.GatewayNamespace,
		"--gateway-name":      cfg.GatewayName,
		"--gateway-uid":       cfg.GatewayUID,
		"--state-file":        cfg.StateFile,
		"--report-file":       cfg.ReportFile,
	} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("required values are missing: %s", strings.Join(missing, ", "))
	}
	if cfg.MemberPort < 1 || cfg.MemberPort > 65535 {
		return errors.New("--member-port must be between 1 and 65535")
	}
	if cfg.ListenerPort < 1 || cfg.ListenerPort > 65535 {
		return errors.New("--listener-port must be between 1 and 65535")
	}
	for flagName, value := range map[string]string{
		"--health-path":  cfg.HealthPath,
		"--match-path":   cfg.MatchPath,
		"--request-path": cfg.RequestPath,
	} {
		if !strings.HasPrefix(value, "/") {
			return fmt.Errorf("%s must start with /", flagName)
		}
	}
	if cfg.OperationTimeout <= 0 || cfg.PollInterval <= 0 || cfg.TrafficTimeout <= 0 {
		return errors.New("all timeouts and polling intervals must be positive")
	}
	if filepath.Clean(cfg.StateFile) == filepath.Clean(cfg.ReportFile) {
		return errors.New("--state-file and --report-file must be different paths")
	}
	return nil
}

func splitNonEmpty(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}

func envOr(getenv func(string) string, name, fallback string) string {
	if value := strings.TrimSpace(getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(getenv func(string) string, name string, fallback int) int {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(getenv func(string) string, name string, fallback bool) bool {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(getenv func(string) string, name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}
