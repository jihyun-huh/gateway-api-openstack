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
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack"
	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
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
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOptions)))

	controllerConfig := controller.Config{
		ControllerName:    gatewayv1.GatewayController(controllerName),
		ControllerVersion: version,
		ClusterID:         clusterID,
		Provider:          provider,
		VIPSubnetID:       vipSubnetID,
		ExternalNetworkID: externalNetworkID,
		MemberSubnetID:    memberSubnetID,
		MemberMode:        controller.MemberMode(memberMode),
		NodeAddressType:   corev1.NodeAddressType(nodeAddressType),
		HealthPath:        healthPath,
	}
	if err := controllerConfig.Validate(); err != nil {
		return fmt.Errorf("invalid controller configuration: %w", err)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(discoveryv1.AddToScheme(scheme))
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
	serviceClients, err := openstack.NewServiceClients(ctx, openstack.ClientConfig{
		Region:         region,
		Microversion:   microversion,
		CloudsYAMLPath: cloudsYAML,
		CloudName:      cloudName,
		AllowInsecure:  allowInsecure,
	})
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
	graphCoordinator := &controller.GraphCoordinator{}
	if err := (&controller.GatewayClassReconciler{Client: manager.GetClient(), Config: controllerConfig}).SetupWithManager(manager); err != nil {
		return fmt.Errorf("set up GatewayClass controller: %w", err)
	}
	gatewayReconciler := &controller.GatewayReconciler{
		Client:      manager.GetClient(),
		Provider:    openStackProvider,
		Coordinator: graphCoordinator,
		APIReader:   manager.GetAPIReader(),
		Config:      controllerConfig,
	}
	if err := gatewayReconciler.SetupWithManager(manager); err != nil {
		return fmt.Errorf("set up Gateway controller: %w", err)
	}
	routeReconciler := &controller.HTTPRouteReconciler{
		Client:      manager.GetClient(),
		Provider:    openStackProvider,
		Coordinator: graphCoordinator,
		APIReader:   manager.GetAPIReader(),
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

func requireGatewayAPIs(restConfig *rest.Config) error {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create Kubernetes discovery client: %w", err)
	}
	resourceList, err := discoveryClient.ServerResourcesForGroupVersion(gatewayv1.GroupVersion.String())
	if err != nil {
		return fmt.Errorf("required Gateway API v1 CRDs are unavailable; install Gateway API Standard v1.6.1 before starting the controller: %w", err)
	}
	availableResources := make([]string, 0, len(resourceList.APIResources))
	for _, resource := range resourceList.APIResources {
		availableResources = append(availableResources, resource.Name)
	}
	for _, required := range []string{"gatewayclasses", "gateways", "httproutes"} {
		if !slices.Contains(availableResources, required) {
			return fmt.Errorf("required Gateway API resource %s is missing from %s; install Gateway API Standard v1.6.1", required, gatewayv1.GroupVersion)
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
