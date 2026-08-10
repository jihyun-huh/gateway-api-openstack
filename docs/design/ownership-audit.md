# Ownership audit contract

**Status:** Proposed

This document defines the first experimental ownership audit. It is a narrow
operator recovery tool for Phase 2, not a cleanup API. The command may change
while the project is pre-alpha. An accepted ADR is still required before the
command name or report format can be treated as stable.

## Why this is needed

Controller finalization deliberately stops when it cannot prove ownership or
when an OpenStack request fails. That is safer than deleting by name, but it
leaves an operator with state to investigate in both Kubernetes and OpenStack.
The audit provides one report that compares the controller's stored Kubernetes
bindings with resources found in the authenticated OpenStack project.

The report helps answer three questions:

- Which OpenStack resources exactly match a current controller binding?
- Which resources need investigation because their identity is stale,
  contradictory, or no longer has a current binding?
- Which Kubernetes objects have incomplete binding metadata that must be
  restored before the OpenStack results can be interpreted?

The audit does not decide whether a resource is safe to delete. It never
adopts, changes, or deletes an object, and it never removes a finalizer.

## Observation model

The command reads all Gateways and HTTPRoutes with binding metadata for the
configured controller. A binding remains relevant while an object is being
deleted or after its GatewayClass is handed to another controller because this
controller may still owe cleanup. Unbound objects are ignored.

Incomplete bindings and bindings for another cluster or OpenStack project are
reported as Kubernetes issues. They are not used as ownership evidence. If any
such issue exists, the report assessment is `incomplete`. In that state, an
`orphanCandidate` result must not be used to justify deletion because the
missing binding may belong to that resource.

The command then lists resources with this controller's cluster and controller
identity in Octavia and Neutron. Descendants are followed only from parents
whose scope can be verified. Every request is a read operation, and all pages
are consumed before the report is produced. If a required Kubernetes or
OpenStack list fails, the command exits without writing a partial report.

Kubernetes and OpenStack do not provide one transaction across both systems.
Resources can change between list operations. Operators should let normal
reconciliation settle and run the audit again before taking any recovery
action.

## Experimental report

The JSON report uses `formatVersion: v1alpha1`. This value deliberately does
not choose the project's future API domain. The report contains:

- the generation time, controller name, and cluster ID
- an assessment of `complete` or `incomplete`
- counts for Kubernetes bindings, binding issues, and OpenStack dispositions
- Kubernetes object references for binding issues and matched findings
- OpenStack resource and parent IDs needed for local investigation
- fixed advisories and known discovery limitations

The OpenStack project ID, controller ownership tuples, raw tags, descriptions,
IP addresses, credentials, API response bodies, and provider error details are
not serialized. Resource IDs, Kubernetes namespaces, names, UIDs, the cluster
ID, and the controller name are still identifiers. Reports are intended for
local operator use and must be reviewed and sanitized before they are attached
to a public issue.

The initial dispositions have these meanings:

- `matched` means the resource and its observed parent relationship match one
  current binding exactly.
- `orphanCandidate` means a complete identity in this controller's scope has no
  current valid binding. It is a prompt for investigation, not deletion
  authority.
- `staleUID` means the Kubernetes names match but at least one immutable UID is
  different.
- `ownershipConflict` means identity, project, parent, or cardinality evidence
  contradicts itself.
- `unresolved` means the available state is not sufficient to reach one of the
  other conclusions safely.

## Known limits

The audit cannot find a resource after all of its controller scope metadata is
removed. Members and L7 rules can only be found through a trusted ancestor.
An unmatched detached Floating IP cannot be attributed from its description
digest alone and is omitted. The audit also does not prove that every current
Kubernetes binding has a complete desired OpenStack graph.

These limits make broad project cleanup unsafe. Recovery remains a manual,
resource by resource process with ownership checks immediately before any
mutation.

## Follow-up decisions

Before this command is declared stable, an ADR must decide its compatibility
policy, long-term format identifier, support window, and whether machine
consumers are in scope. Any future cleanup command requires a separate design
and must repeat live ownership validation at mutation time. It cannot infer
authority from a saved audit report.
