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

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	controllermetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayconsts "sigs.k8s.io/gateway-api/pkg/consts"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack"
	openstackobservability "github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack/observability"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller/gateway"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller/gatewayclass"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller/graph"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller/httproute"
)

// version is overridden at build time with -X main.version=<release>.
var version = "dev"

var setupLog = ctrl.Log.WithName("setup")

func main() {
	if err := run(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Could not run controller manager")
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	resyncDefault, err := durationEnvOr("GATEWAY_OPENSTACK_RESYNC_INTERVAL", controller.DefaultOpenStackResyncInterval)
	if err != nil {
		return err
	}
	apiQPSDefault, err := float64EnvOr("GATEWAY_OPENSTACK_API_QPS", openstack.DefaultAPIQPS)
	if err != nil {
		return err
	}
	apiBurstDefault, err := intEnvOr("GATEWAY_OPENSTACK_API_BURST", openstack.DefaultAPIBurst)
	if err != nil {
		return err
	}
	var (
		controllerName    string
		clusterID         string
		provider          string
		vipSubnetID       string
		externalNetworkID string
		memberSubnetID    string
		memberMode        string
		nodeAddressType   string
		healthPath        string
		region            string
		microversion      string
		cloudsYAML        string
		cloudName         string
		metricsAddress    string
		healthAddress     string
		leaderElection    bool
		allowInsecure     bool
		operationTimeout  time.Duration
		pollInterval      time.Duration
		resyncInterval    time.Duration
		apiQPS            float64
		apiBurst          int
	)
	zapOptions := zap.Options{Development: false}
	zapOptions.BindFlags(flag.CommandLine)
	flag.StringVar(&controllerName, "controller-name", envOr("GATEWAY_OPENSTACK_CONTROLLER_NAME", ""), "Gateway API controllerName (required, for example example.net/gateway-api-openstack)")
	flag.StringVar(&clusterID, "cluster-id", envOr("GATEWAY_OPENSTACK_CLUSTER_ID", ""), "stable unique identifier for this Kubernetes cluster (required)")
	flag.StringVar(&provider, "octavia-provider", envOr("GATEWAY_OPENSTACK_OCTAVIA_PROVIDER", "amphora"), "Octavia provider name")
	flag.StringVar(&vipSubnetID, "vip-subnet-id", envOr("GATEWAY_OPENSTACK_VIP_SUBNET_ID", ""), "OpenStack subnet used for Octavia VIPs (required)")
	flag.StringVar(&externalNetworkID, "external-network-id", envOr("GATEWAY_OPENSTACK_EXTERNAL_NETWORK_ID", ""), "Neutron external network used to allocate a Floating IP")
	flag.StringVar(&memberSubnetID, "member-subnet-id", envOr("GATEWAY_OPENSTACK_MEMBER_SUBNET_ID", ""), "OpenStack subnet containing Kubernetes Node addresses")
	flag.StringVar(&memberMode, "member-mode", envOr("GATEWAY_OPENSTACK_MEMBER_MODE", string(controller.MemberModeNodePort)), "backend member discovery mode; Phase 1 supports NodePort")
	flag.StringVar(&nodeAddressType, "node-address-type", envOr("GATEWAY_OPENSTACK_NODE_ADDRESS_TYPE", string(corev1.NodeInternalIP)), "Node address type used for Octavia members: InternalIP or ExternalIP")
	flag.StringVar(&healthPath, "health-path", envOr("GATEWAY_OPENSTACK_HEALTH_PATH", "/"), "HTTP health monitor path")
	flag.StringVar(&region, "openstack-region", envOr("OS_REGION_NAME", ""), "OpenStack region; defaults to clouds.yaml or OS_REGION_NAME")
	flag.StringVar(&microversion, "octavia-microversion", "2.5", "Octavia API microversion; Phase 1 requires resource tags")
	flag.StringVar(&cloudsYAML, "clouds-yaml", envOr("OS_CLIENT_CONFIG_FILE", ""), "path to clouds.yaml; when empty, standard OS_* variables are used")
	flag.StringVar(&cloudName, "openstack-cloud", envOr("OS_CLOUD", ""), "clouds.yaml entry selected with --clouds-yaml")
	flag.StringVar(&metricsAddress, "metrics-bind-address", ":8080", "metrics endpoint bind address")
	flag.StringVar(&healthAddress, "health-probe-bind-address", ":8081", "health probe bind address")
	flag.BoolVar(&leaderElection, "leader-elect", true, "enable leader election")
	flag.BoolVar(&allowInsecure, "insecure", false, "disable OpenStack TLS verification; test clouds only")
	flag.DurationVar(&operationTimeout, "openstack-operation-timeout", 10*time.Minute, "maximum duration of one OpenStack reconciliation call")
	flag.DurationVar(&pollInterval, "openstack-poll-interval", 2*time.Second, "delay before observing an asynchronous Octavia operation again")
	flag.DurationVar(&resyncInterval, "openstack-resync-interval", resyncDefault, "interval for observing converged OpenStack resources again")
	flag.Float64Var(&apiQPS, "openstack-api-qps", apiQPSDefault, "maximum average OpenStack API requests per second for this process")
	flag.IntVar(&apiBurst, "openstack-api-burst", apiBurstDefault, "maximum burst of OpenStack API requests for this process")
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOptions)))
	openStackClientConfig := openstack.ClientConfig{
		Region:         region,
		Microversion:   microversion,
		CloudsYAMLPath: cloudsYAML,
		CloudName:      cloudName,
		AllowInsecure:  allowInsecure,
		APIQPS:         apiQPS,
		APIBurst:       apiBurst,
	}
	if err := openStackClientConfig.Validate(); err != nil {
		return fmt.Errorf("invalid OpenStack client configuration: %w", err)
	}

	controllerConfig := controller.Config{
		ControllerName:          gatewayv1.GatewayController(controllerName),
		ControllerVersion:       version,
		ClusterID:               clusterID,
		Provider:                provider,
		VIPSubnetID:             vipSubnetID,
		ExternalNetworkID:       externalNetworkID,
		MemberSubnetID:          memberSubnetID,
		MemberMode:              controller.MemberMode(memberMode),
		NodeAddressType:         corev1.NodeAddressType(nodeAddressType),
		HealthPath:              healthPath,
		OpenStackResyncInterval: resyncInterval,
	}
	if err := controllerConfig.Validate(); err != nil {
		return fmt.Errorf("invalid controller configuration: %w", err)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(discoveryv1.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))

	restConfig := ctrl.GetConfigOrDie()
	if err := requireGatewayAPIs(restConfig); err != nil {
		return err
	}
	manager, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                        scheme,
		Metrics:                       metricsserver.Options{BindAddress: metricsAddress},
		HealthProbeBindAddress:        healthAddress,
		LeaderElection:                leaderElection,
		LeaderElectionID:              "gateway-api-openstack-controller",
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return fmt.Errorf("create controller manager: %w", err)
	}
	if err := configureOpenStackRequestMetrics(&openStackClientConfig, controllermetrics.Registry); err != nil {
		return err
	}
	serviceClients, err := openstack.NewServiceClients(ctx, openStackClientConfig)
	if err != nil {
		return err
	}
	controllerConfig.OpenStackProjectID = serviceClients.ProjectID
	openStackProvider := openstack.NewProvider(serviceClients, openstack.ProviderConfig{
		OperationTimeout: operationTimeout,
		PollInterval:     pollInterval,
	})
	if err := controller.SetupIndexes(ctx, manager.GetFieldIndexer(), controllerConfig); err != nil {
		return fmt.Errorf("set up controller field indexes: %w", err)
	}
	graphCoordinator := &graph.Coordinator{}
	eventRecorder := manager.GetEventRecorder("gateway-api-openstack-controller")
	if err := (&gatewayclass.Reconciler{Client: manager.GetClient(), APIReader: manager.GetAPIReader(), Config: controllerConfig}).SetupWithManager(manager); err != nil {
		return fmt.Errorf("set up GatewayClass controller: %w", err)
	}
	gatewayReconciler := &gateway.Reconciler{
		Client:      manager.GetClient(),
		Provider:    openStackProvider,
		Coordinator: graphCoordinator,
		APIReader:   manager.GetAPIReader(),
		Recorder:    eventRecorder,
		Config:      controllerConfig,
	}
	if err := gatewayReconciler.SetupWithManager(manager); err != nil {
		return fmt.Errorf("set up Gateway controller: %w", err)
	}
	routeReconciler := &httproute.Reconciler{
		Client:      manager.GetClient(),
		Provider:    openStackProvider,
		Coordinator: graphCoordinator,
		APIReader:   manager.GetAPIReader(),
		Recorder:    eventRecorder,
		Config:      controllerConfig,
	}
	if err := routeReconciler.SetupWithManager(manager); err != nil {
		return fmt.Errorf("set up HTTPRoute controller: %w", err)
	}
	if err := manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add health check: %w", err)
	}
	if err := manager.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("add readiness check: %w", err)
	}
	setupLog.Info(
		"Starting controller manager",
		"version", version,
		"controllerName", controllerConfig.ControllerName,
		"clusterID", controllerConfig.ClusterID,
		"octaviaProvider", controllerConfig.Provider,
		"memberMode", controllerConfig.MemberMode,
	)
	if err := manager.Start(ctx); err != nil {
		return fmt.Errorf("run controller manager: %w", err)
	}
	return nil
}

func configureOpenStackRequestMetrics(config *openstack.ClientConfig, registerer prometheus.Registerer) error {
	requestMetrics, err := openstackobservability.RegisterRequestMetrics(registerer)
	if err != nil {
		return fmt.Errorf("register OpenStack request metrics: %w", err)
	}
	config.RequestObserver = requestMetrics
	return nil
}

func requireGatewayAPIs(restConfig *rest.Config) error {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create Kubernetes discovery client: %w", err)
	}
	resourceList, err := discoveryClient.ServerResourcesForGroupVersion(gatewayv1.GroupVersion.String())
	if err != nil {
		return fmt.Errorf("required Gateway API v1 CRDs are unavailable; install Gateway API Standard %s before starting the controller: %w", gatewayconsts.BundleVersion, err)
	}
	availableResources := make([]string, 0, len(resourceList.APIResources))
	for _, resource := range resourceList.APIResources {
		availableResources = append(availableResources, resource.Name)
	}
	for _, required := range []string{"gatewayclasses", "gateways", "httproutes"} {
		if !slices.Contains(availableResources, required) {
			return fmt.Errorf("required Gateway API resource %s is missing from %s; install Gateway API Standard %s", required, gatewayv1.GroupVersion, gatewayconsts.BundleVersion)
		}
	}
	return nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func durationEnvOr(name string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	return duration, nil
}

func float64EnvOr(name string, fallback float64) (float64, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	return parsed, nil
}

func intEnvOr(name string, fallback int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	return parsed, nil
}
