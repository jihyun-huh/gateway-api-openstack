# Development package and file layout

Status: current guidance and a proposed later package layout

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

This repository should copy the service boundary, not CAPO's exact dependency
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

## Layout used now

For the current Phase 2 controller, keep one `internal/controller` package and
split each resource by lifecycle responsibility. The tree below shows the
relevant Gateway and HTTPRoute lifecycle files, not every file in the package:

```text
internal/controller/
├── gateway_controller.go
├── gateway_validation.go
├── gateway_binding.go
├── gateway_graph.go
├── gateway_cleanup.go
├── gateway_status.go
├── gateway_version_status.go
├── gateway_scope.go
├── gateway_watch.go
├── httproute_controller.go
├── httproute_parent.go
├── httproute_selection.go
├── httproute_model.go
├── httproute_backend.go
├── httproute_binding.go
├── httproute_graph.go
├── httproute_cleanup.go
├── httproute_status.go
├── httproute_version_status.go
├── httproute_scope.go
└── httproute_watch.go
```

This is intentionally one package for now. HTTPRoute reconciliation consumes
Gateway validation and binding rules, status code uses shared ParentRef
comparison, and the ownership audit reads both bindings. Moving these files
into kind packages today would require exporting implementation details or
creating a vague shared package.

Use these file rules:

- `*_controller.go` contains immutable dependencies, RBAC markers, `Reconcile`,
  and lifecycle dispatch.
- `*_validation.go` and `*_model.go` validate input and build provider-neutral
  values.
- `*_binding.go` owns durable identity metadata and finalizers.
- `*_cleanup.go` contains detach and finalization phases.
- `*_status.go` calculates conditions and ParentStatus entries.
- `*_scope.go` owns request-local state and deferred patching.
- `*_watch.go` contains manager setup and event mapping.

Do not add `common`, `helpers`, or a general controller `util` package. Leave a
small shared function in the controller package until its actual responsibility
has a stable name.

## Package split after the graph decision

[ADR 0001](design/adr/0001-gateway-graph-writer.md) proposes a stable graph
compiler and writer boundary. If that ADR is accepted and the interface survives
implementation review, the controller can move toward:

```text
internal/controller/
├── gatewayclass/
├── gateway/
├── httproute/
├── graph/
└── ownershipaudit/

internal/cloud/
├── graph.go
├── outcome.go
└── errors.go
```

The `graph` package is justified only if it can own a provider-neutral desired
graph and writer without importing a resource reconciler package. Gateway and
HTTPRoute packages may depend on graph contracts; graph must not depend back on
them.

Move a controller into its own package only when all of these are true:

- its lifecycle and status ownership are clear
- shared invariants have a named lower-level owner
- moving it does not create an import cycle
- most identifiers can stay unexported
- its package tests can construct it without another controller's private state

## OpenStack adapter target

The current `internal/cloud/openstack` package keeps the Kubernetes boundary
clean, but graph orchestration, Octavia resources, Neutron Floating IPs, and the
read-only audit now share one directory. After the graph provider interface is
accepted, use service packages and resource files:

```text
internal/cloud/openstack/
├── provider.go
├── clients.go
├── apierrors/
│   └── classify.go
├── identity/
│   └── metadata.go
├── graph/
│   ├── observe.go
│   ├── plan.go
│   ├── mutate.go
│   └── delete.go
├── octavia/
│   ├── service.go
│   ├── loadbalancer.go
│   ├── listener.go
│   ├── pool.go
│   ├── member.go
│   ├── monitor.go
│   ├── l7policy.go
│   └── l7rule.go
├── neutron/
│   └── floatingip.go
└── audit/
    ├── scan.go
    └── classification.go
```

Octavia resources are files in one `octavia` package, not one package per API
resource. Octavia and Neutron are package boundaries because they have
different clients and failure semantics. The graph package composes them and
executes one deterministic mutation. The audit package depends on read-only
interfaces and must not gain a mutation client.

The leaf packages for API error classification and immutable identity need to
come first. Service packages can depend on those leaves, while the root adapter
can depend on the service and graph packages. A child package must not import
the root `openstack` package; that would create an import cycle as soon as the
root provider wires the child. If identity or error handling cannot be given a
small named contract, keep the adapter in one package until it can.

Do this as separate moves after the graph contract is stable. Moving files,
changing the provider interface, and changing ownership behavior in one commit
makes review and rollback much harder.
