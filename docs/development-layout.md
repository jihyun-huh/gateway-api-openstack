# Development package and file layout

Status: current guidance

This page explains how files in the controller and OpenStack adapter should be
split. It is based on current upstream trees, but it is not a rule that every
upstream repository follows in exactly the same way.

There is no useful maximum file length for this project. A long file is a sign
to look for mixed responsibilities. It is not by itself a reason to add a
package. New packages add imports, exported identifiers, interfaces, and the
possibility of dependency cycles, so the boundary needs a name and a stable
reason.

## Upstream patterns

### Kubernetes

Kubernetes normally puts a controller kind in its own package below
`pkg/controller`. The Deployment controller then divides work inside that
package by behavior:

```text
pkg/controller/deployment/
├── deployment_controller.go
├── sync.go
├── rolling.go
├── recreate.go
├── rollback.go
├── progress.go
├── config/
└── util/
```

The [controller tree](https://github.com/kubernetes/kubernetes/tree/b3bc2ac58fa173967f27ade80f28cc5015b8c1c3/pkg/controller)
and [Deployment package](https://github.com/kubernetes/kubernetes/tree/b3bc2ac58fa173967f27ade80f28cc5015b8c1c3/pkg/controller/deployment)
show that the package follows the controller responsibility. Files inside it
follow sync, rollout, rollback, and progress work. A smaller controller can
remain one controller file and one test file.

The Kubernetes coding conventions also warn against package sprawl and broad
`util` packages. Directory and package names should match, and a package name
should say what responsibility it owns. See the
[directory and file conventions](https://github.com/kubernetes/community/blob/main/contributors/guide/coding-conventions.md#directory-and-file-conventions).

### Cluster API

Cluster API gives each core reconciler its own package:

```text
core/reconcilers/
├── cluster/
├── clusterclass/
├── clusterresourceset/
├── machine/
├── machinedeployment/
├── machinehealthcheck/
├── machinepool/
├── machineset/
└── topology/
```

Within one package, it keeps the entry point, phases, status, and focused
algorithms in separate files. The Cluster reconciler is a useful small example:

```text
cluster/
├── cluster_controller.go
├── cluster_controller_phases.go
├── cluster_controller_status.go
├── cluster_controller_test.go
├── cluster_controller_phases_test.go
└── cluster_controller_status_test.go
```

See the pinned [reconciler tree](https://github.com/kubernetes-sigs/cluster-api/tree/8eaa89a5d029f1f1a4325fd20caf52455f7e3e5f/core/reconcilers),
[Cluster reconciler](https://github.com/kubernetes-sigs/cluster-api/tree/8eaa89a5d029f1f1a4325fd20caf52455f7e3e5f/core/reconcilers/cluster),
and [Machine reconciler](https://github.com/kubernetes-sigs/cluster-api/tree/8eaa89a5d029f1f1a4325fd20caf52455f7e3e5f/core/reconcilers/machine).

Cluster API's topology reconciler is the closer example for the future Gateway
graph writer. It keeps current-state observation, desired-state generation,
and reconciliation separate. Its main flow is
`getCurrentState -> Generate -> reconcileState`. See the
[topology reconciliation flow](https://github.com/kubernetes-sigs/cluster-api/blob/8eaa89a5d029f1f1a4325fd20caf52455f7e3e5f/core/reconcilers/topology/cluster/cluster_controller.go#L372-L409).

### Cluster API Provider OpenStack

CAPO's controller directory is relatively flat. Its OpenStack code has a more
useful provider boundary for this repository:

```text
pkg/
├── clients/
│   ├── compute.go
│   ├── networking.go
│   ├── loadbalancer.go
│   └── volume.go
├── cloud/services/
│   ├── compute/
│   ├── networking/
│   └── loadbalancer/
└── scope/
```

Inside networking, files follow resources and operations such as network,
port, router, Floating IP, and security group. See the pinned
[service packages](https://github.com/kubernetes-sigs/cluster-api-provider-openstack/tree/78ef68d5853a85e48e6f6bf69a353b3a1d073526/pkg/cloud/services)
and [networking package](https://github.com/kubernetes-sigs/cluster-api-provider-openstack/tree/78ef68d5853a85e48e6f6bf69a353b3a1d073526/pkg/cloud/services/networking).

This repository uses the service boundary, not CAPO's exact dependency
direction. Kubernetes objects still stop at `internal/controller`, and
Gophercloud types still stop at `internal/cloud/openstack`.

### cloud-provider-openstack

The Octavia Ingress Controller separates its Kubernetes controller from an
OpenStack adapter with Octavia and Neutron files:

```text
pkg/ingress/controller/
├── controller.go
└── openstack/
    ├── client.go
    ├── octavia.go
    └── neutron.go
```

See the pinned [controller](https://github.com/kubernetes/cloud-provider-openstack/blob/ff04631653b624a75c811a0836f2ba4a0395d04e/pkg/ingress/controller/controller.go)
and [OpenStack adapter](https://github.com/kubernetes/cloud-provider-openstack/tree/ff04631653b624a75c811a0836f2ba4a0395d04e/pkg/ingress/controller/openstack).
Some files there are still large. That is another reason not to treat line
count as a community rule.

## Controller layout used now

The controller follows the Cluster API pattern of one package for each core
reconciler. Files inside a resource package still follow lifecycle work. The
tree below leaves out tests and a few small shared files:

```text
internal/controller/
├── config.go
├── binding.go
├── gateway_api_version.go
├── indexes.go
├── predicates.go
├── provider_outcomes.go
├── gatewayclass/
│   ├── gatewayclass_controller.go
│   └── gatewayclass_scope.go
├── gateway/
│   ├── gateway_controller.go
│   ├── gateway_validation.go
│   ├── gateway_binding.go
│   ├── gateway_graph.go
│   ├── gateway_cleanup.go
│   ├── gateway_status.go
│   ├── gateway_scope.go
│   └── gateway_watch.go
├── httproute/
│   ├── httproute_controller.go
│   ├── httproute_parent.go
│   ├── httproute_selection.go
│   ├── httproute_model.go
│   ├── httproute_backend.go
│   ├── httproute_binding.go
│   ├── httproute_graph.go
│   ├── httproute_cleanup.go
│   ├── httproute_status.go
│   ├── httproute_scope.go
│   └── httproute_watch.go
├── graph/
│   └── coordinator.go
└── ownershipaudit/
    └── collector.go
```

The root `controller` package is a small Kubernetes-facing base package. It
owns validated configuration, durable binding parsers, Gateway API version
observation, shared indexes and predicates, and provider outcome policy. It
does not import a reconciler package. This keeps the dependency direction from
the resource packages toward the shared contracts.

The dependencies between resource packages are deliberately small:

```text
gatewayclass ------> controller
gateway -----------> controller, graph, cloud
httproute ---------> controller, graph, gateway, cloud
ownershipaudit ----> controller, audit, cloud
graph -------------> Kubernetes UID keyed coordination only
```

HTTPRoute uses the exported Gateway validation entry point instead of keeping
a second copy of the listener rules. The `graph` package has no dependency on a
resource reconciler. It currently contains only the process-local coordinator
that serializes work by Gateway UID.

This package move does not accept or implement
[ADR 0001](design/adr/0001-gateway-graph-writer.md). In particular,
`internal/controller/graph` is not a desired graph compiler, a route fragment
store, or a single cloud writer. Gateway and HTTPRoute still call the existing
provider operations separately after taking the same keyed lock.

Inside a reconciler package, use these file rules:

- `*_controller.go` contains immutable dependencies, RBAC markers, `Reconcile`,
  and lifecycle dispatch.
- `*_validation.go` and `*_model.go` validate input and build provider-neutral
  values.
- `*_binding.go` owns durable identity metadata and finalizers.
- `*_cleanup.go` contains detach and finalization phases.
- `*_status.go` calculates conditions and ParentStatus entries.
- `*_scope.go` owns request-local state and deferred patching.
- `*_watch.go` contains manager setup and event mapping.

Do not add `common`, `helpers`, or a general controller `util` package. Add a
shared contract to the root package only when more than one resource package
needs it and its ownership is clear.

## OpenStack adapter layout used now

The OpenStack adapter follows CAPO's service-oriented layout, adjusted for the
smaller graph in this repository. Octavia resources remain files in one
package instead of becoming one package per API resource:

```text
internal/cloud/openstack/
├── provider.go
├── compatibility.go
├── wait.go
├── clients/
│   └── client.go
├── apierrors/
│   └── classify.go
├── identity/
│   └── metadata.go
├── octavia/
│   ├── service.go
│   ├── loadbalancer.go
│   ├── listener.go
│   ├── pool.go
│   ├── monitor.go
│   ├── l7policy.go
│   └── wait.go
├── neutron/
│   ├── service.go
│   └── floatingip.go
├── graph/
│   ├── provider.go
│   ├── gateway.go
│   ├── route.go
│   ├── route_graph.go
│   ├── delete.go
│   └── loadbalancer_status.go
└── inventory/
    ├── scanner.go
    └── classification.go
```

The packages have these responsibilities:

- `clients` authenticates once, creates shared service clients, and applies the
  process-wide request limit.
- `apierrors` classifies Gophercloud and HTTP failures into provider-neutral
  cloud errors.
- `identity` encodes and validates immutable ownership metadata.
- `octavia` and `neutron` contain operations for one OpenStack service.
- `graph` composes service operations for the existing Gateway and HTTPRoute
  provider methods, including ownership checks and ordered mutation.
- `inventory` scans through read-only interfaces and builds the advisory
  ownership inventory. It has no mutation interface.

The root `openstack` package is a narrow facade used by commands and probes. It
keeps the existing `cloud.Provider` and audit scanner entry points stable while
delegating to `graph` and `inventory`. A child package never imports the root
facade. Kubernetes and Gateway API types do not enter any OpenStack package.

`internal/cloud/openstack/graph` is not the single Gateway graph writer proposed
by ADR 0001. It is the provider-side home for the existing, separate Gateway
and HTTPRoute operations. The split changes where the code lives, not resource
ownership, route selection, provider interfaces, or mutation behavior.

Use a new service package when it has a distinct OpenStack client and failure
boundary. Keep resources from the same service as files in that package until
they need a smaller contract for a concrete reason. Cross-service identity,
ordering, and ownership checks belong in `graph`, while read-only audit rules
belong in `inventory`.
