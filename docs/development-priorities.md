# Current development priorities

Status: working plan

This page turns the unfinished work in the roadmap into changes that can be
opened as focused issues. It does not replace the phase status and exit gates in
[ROADMAP.md](../ROADMAP.md), and it does not replace an accepted ADR.

## Work before Phase 3

### 1. Finish the Gateway graph writer contract

`gateway.Reconciler` and `httproute.Reconciler` can both change one Octavia load
balancer. The shared `graph.Coordinator` makes those calls take turns inside
the active process. It does not compile the complete Gateway graph and it does
not decide how route fragments survive a restart or a leader change.

Start with an ADR for the writer and route fragment ownership. Preserve the
current route identity and route selection behavior until that decision is
accepted. The implementation should then move toward one provider-neutral
desired Gateway graph, one observed graph, one deterministic mutation plan,
and one cloud mutation entry point. Gateway and HTTPRoute reconciliation can
continue to validate inputs and write the status fields they own.

[ADR 0001](design/adr/0001-gateway-graph-writer.md) is the proposed contract.
It is not accepted yet, so the mutation boundary must not change until its
public review is complete.

The graph lock remains useful, but correctness must come from observation and
desired state rather than memory held by the active process. Tests should run
Gateway and HTTPRoute events in both orders, cancel a waiting reconcile, build
a new reconciler after a partial transition, and show that a second pass over
converged state makes no cloud mutation.

### 2. Protect the controller and OpenStack package boundaries

The behavior-preserving package split is complete. Each reconciler now owns its
lifecycle in a resource package:

- `internal/controller/gatewayclass`
- `internal/controller/gateway`
- `internal/controller/httproute`

The root `internal/controller` package contains the shared Kubernetes-facing
contracts. `internal/controller/graph` contains only the Gateway UID keyed
coordinator, and `internal/controller/ownershipaudit` contains the read-only
Kubernetes binding collector. Resource packages depend on those lower-level
contracts; the root package does not import a reconciler.

The OpenStack adapter is also divided by responsibility. Authentication and
shared clients, error classification, immutable identity, Octavia, Neutron,
cross-service graph operations, and read-only inventory each have a named
package below `internal/cloud/openstack`. The root package remains the facade
used by commands, probes, `cloud.Provider`, and `audit.Scanner`.

The [development layout guide](development-layout.md) records the current
package layout and the upstream patterns behind it. Keep these boundaries in
normal review: controller packages must not import Gophercloud, OpenStack
packages must not import Kubernetes types, and an OpenStack child package must
not import the root facade.

This move does not change the provider interface, ownership rules, selected
route behavior, or mutation ordering. It also does not implement
[ADR 0001](design/adr/0001-gateway-graph-writer.md). The proposed single writer
and durable route fragment contract still need public review before their
implementation begins.

### 3. Tighten event fan-out and retry evidence

The current indexes avoid cluster-wide dependency scans. Node events can still
fan out to every indexed HTTPRoute with a Service backend and then read its
parent, Service, and EndpointSlices to find the affected set. Add scale tests that
count Kubernetes reads and effective reconciles during Node and EndpointSlice
bursts. Narrow the index or mapper if those results exceed the documented API
budget.

Finalization uses typed provider outcomes and workqueue backoff. Add a repeated
failure test that demonstrates the intended change from a bounded quick retry
period to a slow, stable recheck. Cover a resource disappearing between
observation and mutation so lifecycle-specific `404` handling does not leave a
stale success condition.

Audit RBAC against actual write calls at the same time. The controller currently
patches its objects, while RBAC also grants `update` on several main and status
resources. Remove a verb only after API server tests show that metadata,
status, and finalizer patches still work through every lifecycle path.

### 4. Publish the first OpenStack controller report

The first report is a gate, not optional release polish. Run the controller in
a disposable Amphora project and complete the
[OpenStack E2E report template](reports/openstack-e2e-template.md), including
real traffic, restart, no-op, fault, finalization, audit, and leak results.
Store the durable report in the repository. A GitHub issue can collect the
work, but an issue alone is not compatibility evidence for a release. Evidence
terms and support boundaries are defined in the roadmap and
[compatibility matrix](providers/compatibility.md).

### 5. Publish a conformance gap report

Run the pinned GATEWAY-HTTP suite as a gap analysis even while failures are
expected. Classify each failure as a controller defect, compiler work, missing
backend mode, Octavia API limitation, or intentional non-goal. In particular,
settle the path for ClusterIP Services, request header modification, request
redirect, backend weights, invalid backends, and ReferenceGrant combinations.

Keep a local gap report separate from an upstream conformance report. An entry
in the Gateway API implementation list requires a current upstream report. A
stable documented subset and an upstream listing are related goals, but the
roadmap must not imply that one automatically proves the other.

### 6. Decide the public identity before adding a CRD

Accept an ADR for the canonical controller name, API domain, module and
repository ownership, and artifact registry. Include a migration plan from the
current personal module path and operator-selected example controller name.
This is an upgrade safety decision: finalizer and annotation keys are derived
from the configured controller name, so changing it can make a new process miss
old bindings. Do this before publishing the Phase 3 CRD or a long-lived image
reference.

After that decision, publish a versioned pre-alpha image early enough for
outside testing. Pin examples and reports to an immutable digest. The image is
an evaluation artifact; signatures, provenance, SBOMs, multi-architecture
promotion, and a supported Helm chart remain later release work.

## Community work alongside the code

Do not wait for Phase 7 to find the first users and reviewers. The near-term
community work is small and concrete:

- turn this page and the roadmap gates into focused public issues
- keep `good first issue` tasks usable without an OpenStack cloud
- ask Amphora operators for reproducible environment reports
- record design decisions in public ADRs
- seek reviews from Gateway API, cloud-provider-openstack, Gophercloud, and
  Octavia contributors where their interfaces are involved
- publish adopter and upgrade evidence only with the adopter's agreement

The first realistic upstream goal is an honest Gateway API implementation
entry backed by a current conformance report. The criteria for maintainer
growth and any future community home are in [GOVERNANCE.md](../GOVERNANCE.md),
not in this work list.

Repository operations need a small hardening pass as well. Pin every GitHub
Action to a reviewed commit, run race and envtest checks for the exact release
candidate, and add documentation link, license header, and Kustomize render
checks when their maintenance cost is understood. Verify branch protection and
GitHub private vulnerability reporting in repository settings; a file in the
source tree cannot prove either setting is active. Remove merged remote feature
branches only as a separate repository maintenance task.

## Work that should not be pulled forward

Do not use the Phase 2 hardening as a reason to add broad features before the
gates above close. In particular:

- Do not add the GatewayClass configuration CRD before the identity, snapshot,
  migration, and compatibility ADRs are accepted.
- Do not change the selected-route or route identity behavior incidentally
  while building the graph writer.
- Do not attach a managed security group to worker ports without an accepted
  shared-ownership ADR and explicit opt-in.
- Do not add terminated HTTPS before Barbican identity, rotation, rollback, and
  deletion are designed.
- Do not add OVN, resource adoption, Ingress reconciliation, or an in-cluster
  proxy mode to the core controller.
