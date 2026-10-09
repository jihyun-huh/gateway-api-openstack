# Current development priorities

Status: Phase 3 prerequisite work and API design

This page turns the unfinished work in the roadmap into changes that can be
opened as focused issues. It does not replace the phase status and exit gates in
[ROADMAP.md](../ROADMAP.md), and it does not replace an accepted ADR.

Phase 2 is complete as a development milestone following the baseline OpenStack E2E run for the constrained HTTP and NodePort path.
The [development validation record](providers/compatibility.md#development-validation) describes the tested revision and remaining limitations.
The [work carried into Phase 3](../ROADMAP.md#work-carried-into-phase-3) remains open.

## Phase 3 work

Begin with the Gateway graph writer, the public identity and class API contracts, and worker NodePort member selection.
Accept the relevant ADRs before changing ownership or implementing the class API.

### 1. Finish the Gateway graph writer contract

`gateway.Reconciler` and `httproute.Reconciler` can both change one Octavia load
balancer. The shared `graph.Coordinator` makes those calls take turns inside
the active process. It does not compile the complete Gateway graph and it does
not decide how route fragments survive a restart or a leader change.

Start with an ADR for the writer and route fragment ownership.
Preserve the current route identity and route selection behavior until that decision is accepted.
The implementation should then move toward one provider-neutral desired Gateway graph, one observed graph, one deterministic mutation plan, and one cloud mutation entry point.
Keep observation and execution separate from pure model construction and diff calculation so tests and benchmarks can exercise those calculations without Kubernetes or OpenStack I/O.
Gateway and HTTPRoute reconciliation can continue to validate inputs and write the status fields they own.

[ADR 0001](design/adr/0001-gateway-graph-writer.md) is the proposed contract.
It is not accepted yet, so the mutation boundary must not change until its
public review is complete.

The graph lock remains useful, but correctness must come from observation and desired state rather than memory held by the active process.
Tests should run Gateway and HTTPRoute events in both orders, cancel a waiting reconcile, build a new reconciler after a partial transition, and show that a second pass over converged state makes no cloud mutation.

Add regression tests for [backend transitions](design/architecture.md#backend-transitions) that inspect each intermediate mutation and observed graph:

- Switch between ready NodePort backends while preserving the pool, L7 policy, and unchanged member identities.
- Change a backend NodePort or replace the complete member set and record any intermediate empty pool caused by the current member deletion order.
- Remove a backend or its ready endpoints, then restore it and verify the status, cleanup, and recovery sequence.
- Interrupt a transition with `PENDING_*`, a timeout, a restart, or another spec change and verify revalidation and eventual convergence with at most one mutation per reconciliation.

### 2. Protect the controller and OpenStack package boundaries

The behavior-preserving package split is complete. Each reconciler now owns its
lifecycle in a resource package:

- `internal/controller/gatewayclass`
- `internal/controller/gateway`
- `internal/controller/httproute`

The root `internal/controller` package contains the shared Kubernetes-facing contracts.
`internal/controller/graph` contains only the Gateway UID keyed coordinator, and `internal/controller/ownershipaudit` contains the read-only Kubernetes binding collector.
Resource packages depend on those lower-level contracts.
The root package does not import a reconciler.

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

### 3. Measure reconciliation cost and tighten event fan-out

The current indexes avoid cluster-wide dependency scans.
Node events can still fan out to every indexed HTTPRoute with a Service backend and then read its parent, Service, and EndpointSlices to find the affected set.
Use synthetic fixtures, fake readers and adapters, and local benchmarks to establish a baseline before changing indexes, caches, or worker concurrency.
Count Kubernetes reads, enqueued objects, effective reconciles, and member mutations during Node and EndpointSlice bursts, and measure model construction time and allocations as fixture sizes grow.
Use the results to set an API budget and identify which work needs to be narrowed.

Start with existing controller-runtime and OpenStack request metrics, then add measurements only where they leave a gap.
Separate model calculation, Gateway lock wait, client-side rate-limit wait, API latency, Octavia pending duration, and total convergence time.
Keep metric labels bounded and exclude object names, UIDs, and cloud IDs.
Run these scale measurements locally rather than generating load in a shared OpenStack project.

Reuse dependency reads within one reconciliation snapshot where measurements justify it.
Any narrower member calculation still submits work through the Gateway graph writer and preserves complete ownership validation and live mutation checks.
Do not introduce an independent member writer or use cached fragments as authority for deletion.

Finalization uses typed provider outcomes and workqueue backoff. Add a repeated
failure test that demonstrates the intended change from a bounded quick retry
period to a slow, stable recheck. Cover a resource disappearing between
observation and mutation so lifecycle-specific `404` handling does not leave a
stale success condition.

Audit RBAC against actual write calls at the same time. The controller currently
patches its objects, while RBAC also grants `update` on several main and status
resources. Remove a verb only after API server tests show that metadata,
status, and finalizer patches still work through every lifecycle path.

### 4. Complete the remaining OpenStack evidence

Use the [OpenStack E2E test guide](testing-openstack-e2e.md) to extend the baseline evidence with the remaining fault, upgrade, and operator recovery scenarios, and rerun the baseline after controller changes.
Use the Local backend profile to restrict test placement to selected Nodes.
Label-based test placement remains necessary until the controller implements the [worker NodePort member selection](../ROADMAP.md#worker-nodeport-member-selection) contract.
Use a dedicated environment for fault scenarios under the guide's restrictions.

Follow the [publication policy](reports/README.md) for local artifacts and PR summaries.
Formal compatibility and release claims still require reviewed evidence under the roadmap and [compatibility matrix](providers/compatibility.md).

### 5. Assess conformance after Phase 3 implementation

After completing Phase 3 implementation, run the pinned GATEWAY-HTTP suite as the next validation step and publish a gap report, even while failures are expected.
Classify each failure as a controller defect, compiler work, missing backend mode, Octavia API limitation, or intentional non-goal.
Use the findings to plan follow-up work for ClusterIP Services, request header modification, request redirect, backend weights, invalid backends, and ReferenceGrant combinations.

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

Define class configuration, per-Gateway overrides, snapshots, propagation, and migration before implementing the API.
Specify defaults, override precedence, and conflict status for the concrete infrastructure settings in the roadmap.
The identity, parameter, and compatibility ADRs must be accepted before adding the CRD.

After that decision, publish a versioned pre-alpha image early enough for outside testing.
Pin examples and reports to an immutable digest.
The image is for evaluation.
Signatures, provenance, SBOMs, multi-architecture promotion, and a supported Helm chart remain later release work.

### 7. Define worker NodePort member selection

Make worker Nodes the default for NodePort members under the [roadmap's eligibility rules](../ROADMAP.md#worker-nodeport-member-selection).
Record the control-plane exclusion rules and any opt-in exceptions in a design issue or ADR before implementation.
Cover both traffic policies, the upstream load balancer exclusion label, and changes to Node eligibility without recreating unchanged members.

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

Repository operations need a small hardening pass as well.
Pin every GitHub Action to a reviewed commit, run race and envtest checks for the exact release candidate, and add documentation link, license header, and Kustomize render checks when their maintenance cost is understood.
Verify branch protection and GitHub private vulnerability reporting in repository settings.
A file in the source tree cannot prove either setting is active.
Remove merged remote feature branches only as a separate repository maintenance task.

## Work that should not be pulled forward

Starting Phase 3 does not remove the design requirements for new features:

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
