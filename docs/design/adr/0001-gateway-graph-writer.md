# ADR 0001: One writer for each Gateway graph

Status: Proposed

Date: 2026-08-18

Owners: `jihyun-huh`

Review issue: Not opened. Open and link a public design issue before review or
acceptance.

## Context

A Gateway and its attached HTTPRoute change the same Octavia load balancer.
Today the Gateway reconciler calls `EnsureGateway` and the HTTPRoute reconciler
calls `EnsureRoute`. `GraphCoordinator` makes those calls take turns for one
Gateway UID, but each reconciler still observes a different part of the graph
and creates its own mutation plan.

That arrangement closes the immediate concurrent mutation race. It does not
give the controller one complete desired graph. A restart between route detach
and route attach can also lose the reason that one reconciler intended to
delete a route while another still sees a valid binding and recreates it.

The writer must preserve the current safety rules:

- Gateway and HTTPRoute bindings are durable before cloud creation.
- Status that reports a replacement or rejection is durable before cleanup.
- A route cleanup cannot delete the Gateway load balancer or listener.
- Whole-Gateway deletion validates and removes the complete owned graph.
- One reconciliation performs at most one OpenStack mutation.
- A restart or leader change does not require an in-memory fragment registry.
- Current route selection and exact HTTPRoute identity remain unchanged.

This ADR defines the writer and route fragment contract. It does not add
multiple routes, change precedence, or accept a new public configuration API.

## Decision

### One shared writer

The manager constructs one `GatewayGraphWriter` and passes the same instance to
the Gateway and HTTPRoute reconcilers. The writer owns the Gateway UID
coordinator, final live ownership checks, complete graph construction, and the
only call to the provider's graph reconciliation method.

Reconcilers continue to decide Gateway API acceptance and update only the
status fields they own. They may ask the writer to reconcile a Gateway graph,
but they do not call Gateway or route cloud mutation methods directly.

The cloud boundary becomes one operation shaped like:

```go
type GatewayGraphSpec struct {
    Identity Identity
    Gateway  GatewayGraphNode
    Routes   []RouteGraphNode
}

type Provider interface {
    ReconcileGatewayGraph(
        context.Context,
        GatewayGraphSpec,
    ) (GatewayGraphResult, error)
}
```

The exact Go names may change during implementation. The single graph input,
component results, and one provider mutation entry point are part of this
decision.

### Complete graph and dispositions

The graph has explicit authority for the Gateway and every known route
fragment. A node uses one of these dispositions:

- `Ensure` means the object is desired and its complete provider-neutral model
  is available.
- `Preserve` means the writer may observe and validate the component but must
  not create, update, or delete it.
- `Delete` means a durable Kubernetes checkpoint authorizes deletion of the
  exact owned component.

HTTPRoute finalization uses `Gateway=Preserve` with its exact route fragment set
to `Delete`. It can never delete the load balancer or listener. Gateway
finalization uses `Gateway=Delete` and may remove the complete graph after the
existing whole-graph identity validation succeeds.

If the writer cannot build a complete and unambiguous graph, it holds the graph
without mutation. A partial Kubernetes read, an invalid durable binding, an
unknown route-scoped child, or a fragment whose lifecycle cannot be decided is
not interpreted as permission to clean up.

### Route fragments

A route fragment contains the exact HTTPRoute identity and the desired pool,
member, health monitor, L7 policy, and L7 rule inputs. It does not contain
cached Octavia IDs. Load balancer, Floating IP, and listener fields belong to
the Gateway part of the graph.

For the Phase 2 profile, the graph contains at most one desired route fragment.
The controller keeps the current selection rule and exact route identity tags.
Changing selection, priority across several routes, or shared route resources
requires another ADR.

The writer sorts fragments and every resource derived from them before it
assigns policy positions or builds a provider plan. Duplicate route identities
or two fragments claiming the same owned role are ownership conflicts.

### Durable fragment lifecycle

The controller stores a fragment lifecycle marker with the existing HTTPRoute
binding. It has two durable states:

- `Attached` means the exact route fragment remains part of the desired graph.
- `Detaching` means the rejection, replacement, or deletion checkpoint is
  durable and the exact route fragment must be removed.

A binding written by an older controller has no lifecycle marker. It is read as
`Attached` for upgrade compatibility. If its current desired model cannot be
built, the writer holds the graph. When the ParentRef still exists, the
HTTPRoute reconciler first publishes the appropriate ParentStatus. When the
ParentRef has been removed, it does not invent a new ParentStatus and uses the
existing cleanup reason annotation as the checkpoint. It confirms the relevant
checkpoint with a live read, then writes `Detaching` in a separate
reconciliation. It must not infer deletion from a model error inside the
writer.

`Detaching` is sticky across restart and spec changes. The controller clears it
only after the provider confirms that the exact route graph is absent and the
route binding and finalizers can be removed. A normal object deletion may use
its durable deletion timestamp as the user-visible checkpoint, but it still
records the fragment lifecycle before the provider mutation.

The lifecycle marker is metadata owned by this controller and uses the same
controller-derived domain as the existing binding. Its literal key is defined
with the implementation and follows the controller identity migration policy.

### Live graph construction

The writer acquires the Gateway UID coordinator before its final snapshot. It
then reads the Gateway, GatewayClass, and candidate HTTPRoutes again through the
uncached API reader. It validates UIDs, generations, deletion state, finalizers,
bindings, controller ownership, and the installed Gateway API bundle before the
provider call.

Cached indexes may find candidate keys, but an index over the cache is not
proof that an uncached list is complete. The current same-namespace profile may
list the Gateway namespace through the live reader and filter it. A later
multi-namespace design needs an explicit completeness strategy before it can
use only indexed candidate keys.

Desired fragments are rebuilt from Kubernetes resources on every graph
reconciliation. The writer keeps no route fragment registry across calls.
`GraphCoordinator` remains a process-local serialization tool; it is not the
source of desired state and it does not need recovery after a restart.

A missing Gateway blocks normal `Ensure` work. It does not erase cleanup
authority already recorded on a bound HTTPRoute. When an HTTPRoute has a
complete durable binding and a confirmed `Detaching` marker, and the referenced
Gateway is `NotFound`, the writer builds a cleanup-only graph from that exact
identity. This covers deletion, controller handoff, invalid input, and loss of
route selection after their checkpoint is durable. The writer uses
`Gateway=Preserve` and `Route=Delete`, never creates a component, and never
deletes the load balancer or listener. An incomplete or conflicting binding
still holds the graph without mutation.

### Provider observation and planning

The OpenStack adapter observes the load balancer, Floating IP, listener, and all
known route descendants as one graph. It validates the full desired input and
the complete observed ownership graph before the first mutation.

It builds one deterministic ordered plan. Normal ordering is:

1. Reconcile Gateway prerequisites.
2. Remove a fragment marked `Delete`.
3. Reconcile an attached route fragment.
4. Restore listener administrative state.
5. Restore load balancer administrative state.

Whole-Gateway deletion uses the validated reverse dependency order. The
adapter executes only the first plan step and returns. A later reconciliation
observes the result again. It must not implement the graph call as sequential
opaque calls to the existing `EnsureGateway` and `EnsureRoute` methods, because
that would retain separate observations and could perform two mutations.

During normal `Ensure`, an unexpected route-scoped resource that matches the
Gateway tuple but cannot be assigned to an exact desired or detaching route
identity is an ownership conflict. It is not adopted or garbage collected.

### Component results and status

The graph result has a whole-call outcome for scheduling and separate Gateway
and route component results. Errors identify the affected component when one is
known. A route failure found during a Gateway-triggered write is not reported as
a Gateway provisioning failure.

Gateway and HTTPRoute reconcilers translate those component results into their
existing status contracts. The writer does not patch Kubernetes objects and
does not overwrite another controller's HTTPRoute parent status. A route
transition does not make Gateway `Programmed=False` when the Gateway component
itself remains programmed.

## Consequences

There is one place to enforce complete observation, minimal diff, mutation
ordering, and one-mutation-per-reconcile. Gateway and HTTPRoute event order no
longer selects a different cloud mutation path. The same desired graph can be
rebuilt after process restart.

The change is larger than a wrapper around the current Provider interface. The
adapter needs a combined observation and plan, controller tests need a graph
provider fake, and status handling needs component-scoped results. Reconciliation
may use extra live reads to prove that the candidate set is complete.

The durable lifecycle adds one controller-owned annotation and one extra
checkpoint when a route starts detaching. That cost keeps status-before-cleanup
ordering and restart recovery from depending on process memory.

## Alternatives considered

### Keep the two writers behind the current coordinator

This prevents overlapping calls but leaves two desired snapshots and two cloud
entry points. Restart behavior and global L7 ordering would still depend on
which reconciler runs.

### Add an `EnsureGraph` facade around the current provider methods

Calling `EnsureGateway` and then `EnsureRoute` under another name preserves the
two observations and plans. It can also perform more than one mutation in a
single graph reconciliation. This is not the selected design.

### Store route fragments only in memory

An in-memory registry is simple while one process is running, but a leader
change loses detach intent and fragment completeness. It is not a source of
truth for a level-based controller.

### Add a fragment CRD in Phase 2

A CRD could make fragments durable, but it adds a public API, upgrade contract,
and garbage-collection lifecycle before the GatewayClass API work in Phase 3.
The existing HTTPRoute binding is sufficient for the current one-route profile.

### Let only the Gateway reconciler write and make HTTPRoute status implicit

This removes one caller but makes route finalization and component status hard
to recover without another observation contract. The selected shared writer
keeps one mutation boundary while allowing either reconciler to drive progress.

## Safety and recovery

Every provider call occurs after the Gateway UID lock and a fresh validation of
the complete Kubernetes snapshot. The provider validates immutable project,
cluster, controller, Gateway, route, and resource-role identity before mutation.
Names and cached IDs remain discovery hints only.

After a lost response, timeout, `PENDING_*` state, restart, or leader change,
the next call observes the complete graph again. It never resumes from an
in-memory plan. A foreign or duplicate resource stops all graph mutation with
an ownership conflict.

The `Detaching` marker prevents a Gateway event from recreating a route during
cleanup. Separate Gateway and route dispositions prevent route cleanup from
expanding into whole-Gateway deletion.

## API and upgrade impact

This decision does not change Gateway API resources, route selection, supported
features, or OpenStack identity tags. Existing route bindings remain valid and
are read as `Attached`. The implementation checkpoints the explicit lifecycle
before it removes the old provider methods.

The provider-neutral Go interface changes and is internal to this repository.
The existing controller-derived metadata domain remains in use. Migration to a
canonical controller name or API domain is handled by the separate public
identity ADR.

The lifecycle marker needs a staged rollout. A compatibility release first
learns to read `Detaching` and routes it only through the existing cleanup path;
that release does not write the marker or change the provider boundary. The
writer release can then record `Detaching` and still roll back to the
compatibility release without recreating the route. Downgrading to an earlier
release is allowed only after a preflight confirms that no HTTPRoute binding is
marked `Detaching`. Release notes must state the tested forward and rollback
path and the command used for that preflight.

## Test and evidence plan

Controller tests must cover:

- Gateway and HTTPRoute events in both orders producing the same graph
- binding before the first create and status before `Detaching`
- cancellation while waiting for the Gateway UID lock
- a changed live snapshot after lock acquisition
- restart with a newly constructed writer during partial create and delete
- route cleanup preserving the Gateway component
- a bound `Detaching` HTTPRoute cleanup when the Gateway is `NotFound`
- normal `Ensure` holding when the Gateway is `NotFound`
- whole-Gateway cleanup removing the validated complete graph
- unsupported Gateway API bundle cleanup
- component status and foreign HTTPRoute parent status preservation
- compatibility-release cleanup and downgrade preflight for `Detaching`

OpenStack adapter tests must cover:

- complete desired validation before mutation
- deterministic ordering with an attached and detaching fragment
- at most one mutation when several components need work
- zero mutation on a converged second pass
- pending state, timeout, lost create response, and a new Provider instance
- duplicate, foreign, and unaccounted descendants
- listener then load balancer administrative repair
- whole-graph reverse deletion and lifecycle-specific `404` handling

OpenStack end-to-end evidence remains a separate Phase 2 gate. Accepting this
ADR and passing local tests do not prove compatibility or conformance.

## Follow-up work

- Open the public design issue and review this proposal before acceptance.
- Implement the durable route fragment lifecycle and migration checkpoint.
- Replace the Provider methods with the complete graph operation.
- Move the adapter to one desired graph, observation, plan, and mutation path.
- Retarget current coordinator, transition, deletion, and lost-response tests.
- Publish the Phase 2 OpenStack and conformance gap reports after the writer is
  implemented and reviewed.
