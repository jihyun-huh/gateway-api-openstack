# Roadmap

This document is a working plan, not a release commitment. The order and scope
may change as the controller is implemented and tested against real OpenStack
environments.

The initial API baseline is the Gateway API Standard Channel from v1.6.1. Each
controller release will pin the Gateway API version it supports.

## Scope

The first releases will focus on one path:

- `GatewayClass`, `Gateway`, and `HTTPRoute`
- OpenStack Octavia as the loadbalancing backend
- an independent controller built with `controller-runtime` and Gophercloud
- Amphora as the first Octavia provider exercised by the capability probe
- NodePort members as the first backend connectivity mode, and safe
  coexistence with `cloud-provider-openstack`.

The controller will own only resources created for its Gateway API objects. It
will not reconcile `Service` objects of type `LoadBalancer`, and it will not
adopt resources created by cloud-provider-openstack or another controller.

## Phase 0 - Initialize and design

**Status:** Initial capability probe completed - documentation remains iterative
**Milestone:** `phase-0-initialize`

The purpose of this phase is to test the assumptions behind the controller
before building its reconciliation loops.

### Repository baseline

- Create the public repository as `gateway-api-openstack`.
- Use **Gateway API for OpenStack** as the descriptive project title.
- Add the Apache-2.0 license, contributing guide, code of conduct, and basic
  issue and pull-request templates.
- Reserve the following implementation names:
  - binary and deployment: `openstack-gateway-controller`
  - Helm chart: `openstack-gateway-controller`.
- Choose a project-controlled domain before fixing the controller name or
  introducing a project API group.

### Octavia capability check

Create a small Gophercloud program that can create, inspect, update, and delete:

- load balancer
- HTTP listener
- pool and members
- health monitor
- L7 policies and rules
- Floating IP, when external access is enabled

Record the tested Octavia release, provider, required API features, quotas, and
Barbican availability. The result should be a provider capability document,
not a general claim that every OpenStack cloud is supported.

### Backend connectivity

- Prove traffic from an Octavia load balancer to a worker Node IP and NodePort.
- Record the securitygroup and routing requirements.
- Test Pod IP members only when the Pod network is directly routable from the
  Octavia member network.
- Select one member mode for the MVP and document why it was selected.

### Design decisions

Write the first architecture decision records for:

1. the ownership boundary with cloud-provider-openstack
2. the mapping from Gateway API objects to Octavia resources
3. backend member discovery and connectivity
4. resource identity, finalizers, and deletion safety
5. provider capability handling
6. credentials and controller configuration

The resource identity design must include the cluster, controller, namespace,
name, Gateway UID, and resource role. A resource that does not match the full
identity must never be adopted, changed, or deleted.

### Test environment

Document a reproducible test topology with:

- Kubernetes and Gateway API versions;
- CNI and kube-proxy mode;
- worker, Pod, and Service network ranges;
- Octavia provider and version;
- VIP, member, and external network layout;
- Floating IP and Barbican availability; and
- the OpenStack roles and quotas required by the controller.

Do not include credentials, customer information, or private cloud identifiers
in the repository.

### Done when

- A manually assembled Octavia path can send an HTTP request to a Kubernetes
  backend using the selected member mode.
- The controller and cloud-provider-openstack ownership boundary is written
  down and testable.
- The initial provider capability document is complete.
- The six design decisions have been reviewed.
- The result supports a clear go/no-go decision for the native controller.

## Phase 1 - HTTP MVP

**Status:** In progress
**Milestone:** `v0.1.0`

This phase implements the smallest useful end-to-end controller. It is not
expected to cover the full Gateway API surface.

### Current progress

The source tree now contains the controller manager, the three Phase 1
reconcilers, the Gophercloud provider boundary, identity-safe resource
operations, unit tests, and a minimal Kustomize deployment with a NodePort
example.

The retained Phase 0 probe has exercised the individual Octavia and Neutron
primitives in one environment. Before `v0.1.0`, the assembled controller path
still needs a published real cloud end-to-end result covering traffic,
controller restart, and ordered deletion. A versioned release image is not yet
published.

### Controller foundation

- Scaffold the project with Kubebuilder and `controller-runtime`.
- Use Gophercloud clients directly.
- Keep OpenStack operations behind small internal interfaces so they can be
  tested with fakes.
- Add leader election, health and readiness endpoints, structured logging,
  metrics, and graceful shutdown.
- Fail with a useful message when the required Gateway API CRDs are missing.

### Configuration

- Support Keystone application credentials.
- Support a named cloud from `clouds.yaml`.
- Keep credentials in Kubernetes Secrets and out of status, logs, and events.
- Configure the controller name, cluster identity, Octavia provider, VIP
  network or subnet, external network, and backend member mode.
- Defer a custom `parametersRef` API until the required fields are understood.

### Reconciliation

Implement three reconcilers:

- `GatewayClass`
- `Gateway`
- `HTTPRoute`

Use standard Gateway API conditions for invalid references, unsupported fields,
and OpenStack provisioning failures.

### Resource lifecycle

- Add the identity metadata defined in Phase 0 to every managed resource.
- Use finalizers for Gateway-owned cleanup.
- Make create and delete operations idempotent.
- Wait for Octavia provisioning states instead of issuing overlapping updates.
- Refuse to adopt resources that were not created by this controller.

### Known limitations

- **Amphora** is the only provider exercised by the Phase 0 probe, controller
  E2E validation is still pending.
- HTTP is the only supported listener protocol.
- A Gateway has one listener.
- A Gateway selects one HTTPRoute.
- An HTTPRoute has one rule, at most one exact hostname, at most one exact or
  path-prefix match, and one same-namespace backend.
- NodePort is the only member mode.
- Backend Services must meet the member-mode requirements selected in Phase 0.
- Existing Octavia resources are never adopted.
- Ingress resources are not reconciled.

### Done when

- Applying a `GatewayClass`, `Gateway`, and `HTTPRoute` produces a working
  Octavia HTTP load balancer.
- Traffic reaches the referenced Service backend.
- Status conditions explain both successful and rejected configurations.
- Deleting the Kubernetes objects removes only matching controller-owned
  OpenStack resources.
- Restarting the controller does not create duplicate resources.
- Installation, a basic example, and removal are documented for `v0.1.0`.

## Phase 2 - Lifecycle and failure handling

**Status:** Planned  
**Milestone:** `v0.2.0`

The MVP establishes the resource model. This phase makes that model safe under
the asynchronous and failure/prune conditions seen in real Octavia deployments.

### Reconciliation safety

- Serialize updates that target the same load balancer with a fine-grained
  reconciliation lock.
- Add bounded retries, exponential backoff, and operation timeouts.
- Handle `PENDING_*`, `ERROR`, and unexpectedly missing resources.
- Recover from partial creation, including a listener without its pool or a
  pool without all expected members.
- Detect drift and update only fields owned by the controller.

### Deletion and recovery

- Make cleanup resumable after controller restarts.
- Define behavior for resources that remain stuck during deletion.
- Provide an explicit operator procedure for inspecting and resolving blocked
  finalizers.
- Add an orphan audit command or report that compares Gateway UIDs with managed
  OpenStack resources.

### Operational behavior

- Handle quota exhaustion, authentication failures, rate limits, and temporary
  OpenStack API errors separately.
- Emit Kubernetes events for actions that require operator attention.
- Add metrics for reconciliation duration, OpenStack operations, retries,
  provisioning failures, and managed resource counts.
- Avoid logging credentials, certificates, or application-credential secrets.

### Tests and release packaging

- Add failure-injection tests for every recovery case above.
- Test leader changes and controller restarts during create, update, and delete.
- Test repeated reconciliation without OpenStack changes.
- Add upgrade tests from `v0.1.x`.
- Publish a Helm chart in addition to the Kustomize manifests.

### Done when

- Restart, leader-change, partial-create, quota, timeout, drift, and external
  deletion tests pass.
- Repeated reconciliation does not issue unnecessary Octavia updates.
- A failed cleanup can be diagnosed without direct database access.
- An upgrade from `v0.1.x` preserves existing Gateways and their OpenStack
  resources.

## Phase 3 - TLS and broader HTTPRoute support

**Status:** Planned  
**Milestone:** `v0.3.0`

This phase expands the supported API surface after the ownership and recovery
model is stable.

### Gateway and route support

- Support multiple listeners on a Gateway.
- Support multiple HTTPRoutes attached to a listener.
- Support multiple rules and backend references.
- Implement backend weights when they can be represented without changing
  Gateway API semantics.
- Implement `ReferenceGrant` checks for cross-namespace references.
- Add only Gateway API filters that Octavia can represent correctly. Return an
  explicit unsupported condition for the rest.

### TLS

- Support terminated HTTPS listeners.
- Read certificates from Kubernetes TLS Secrets.
- Store certificate material in Barbican using controller-owned resources.
- Support certificate rotation without replacing the load balancer.
- Remove controller-owned Barbican resources during Gateway cleanup.
- Do not adopt existing Barbican containers or secrets.

### Backend modes

- Keep NodePort as the compatibility mode.
- Add routable Pod IP members when the network topology allows them.
- Reconcile member changes caused by EndpointSlice updates.
- Document the health-monitor and source-IP behavior of each mode.

### Capability and API checks

- Validate required Octavia features before programming a Gateway.
- Reject providers that cannot supply the HTTP and L7 behavior required by the
  requested route.
- Publish `supportedFeatures` only for behavior covered by tests.
- Run the relevant Gateway API HTTP conformance tests in CI. The reports are
  used as an engineering signal and are not a release claim by themselves.

### Done when

- Multiple listeners and routes can share one Gateway without ownership
  conflicts.
- HTTPS works with certificate creation, rotation, and cleanup.
- Cross-namespace references are accepted only when a matching
  `ReferenceGrant` exists.
- NodePort and routable Pod IP modes have separate end-to-end tests.
- Unsupported filters and provider limitations are visible in status.

## Phase 4 - Release hardening

**Status:** Later  
**Milestone:** `v0.4.0`

This phase turns the supported feature set into a repeatable release that can
be operated across more than one OpenStack test topology.

### Compatibility and scale

- Test on at least two independently provisioned Kubernetes-on-OpenStack
  environments with documented network topologies.
- Publish the exact Kubernetes, Gateway API, OpenStack, Octavia, and provider
  versions tested by each release.
- Add scale tests for Gateways, routes, listeners, pools, and members.
- Measure reconciliation time and OpenStack API usage during large endpoint
  changes.
- Add concurrency limits that protect the OpenStack API and Octavia workers.

### Release process

- Produce versioned controller images and Helm charts.
- Sign release artifacts and publish SBOM and provenance data.
- Document installation, upgrade, rollback, recovery, and uninstall procedures.
- Define compatibility rules for controller configuration and any project CRDs.
- Publish troubleshooting guides for common status and OpenStack failure
  states.
- Add an uninstall preflight check that reports managed resources which still
  exist.

### Done when

- The supported end-to-end suite passes in both test environments.
- Scale tests have recorded limits and do not leak OpenStack resources.
- A clean install and an upgrade from each supported minor release are tested.
- Release artifacts are reproducible, signed, and documented.
- The support matrix describes tested combinations rather than claiming
  generic OpenStack compatibility.

## Deferred

The following work is outside the core roadmap until there is a concrete use
case:

- Traefik or Envoy deployment profiles;
- Ingress API reconciliation or automatic Ingress migration;
- adoption of pre-existing Octavia load balancers;
- TCPRoute, UDPRoute, and TLSRoute;
- multi-cluster Gateway;
- GAMMA or service-mesh support;
- arbitrary filters that require an in-cluster L7 proxy;
- provider-specific extensions that cannot be represented by Gateway API; and
- management of an in-cluster proxy data plane by this controller.

## Technical references

- [Gateway API implementer's guide](https://gateway-api.sigs.k8s.io/guides/implementers-guide/)
- [Gateway API v1.6.1 release](https://github.com/kubernetes-sigs/gateway-api/releases/tag/v1.6.1)
- [Gateway API conformance tests](https://github.com/kubernetes-sigs/gateway-api/tree/main/conformance)
- [Octavia provider feature matrix](https://docs.openstack.org/octavia/latest/user/feature-classification/index.html)
- [Gophercloud](https://github.com/gophercloud/gophercloud)
- [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime)
- [cloud-provider-openstack](https://github.com/kubernetes/cloud-provider-openstack)
