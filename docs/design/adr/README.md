# Architecture decision records

Architecture decision records (ADRs) capture decisions that are hard to undo or
that change the project's public or safety contract. They are shorter than a
complete implementation design. The issue and pull request can carry detailed
discussion; the accepted ADR records what the project decided and why.

No ADR has been accepted yet. Existing design documents are proposed direction
unless they describe behavior that is already implemented and tested.

## When an ADR is required

Write an ADR before changing:

- the OpenStack ownership boundary or immutable identity
- the writer and resource mapping for a Gateway graph
- member mode, credentials, or provider capability semantics
- the canonical controller name, module ownership, or API domain
- a public CRD, flag, annotation, report format, or compatibility policy
- shared mutation of a Kubernetes or OpenStack object owned by another
  controller or the operator
- certificate identity and lifecycle
- adoption, deletion recovery, or finalizer guarantees

A cleanup or refactor that preserves these contracts does not need an ADR.
When the boundary is unclear, start with a design issue instead of hiding the
decision in a code review.

## Process

1. Open a design issue that states the user problem, current behavior,
   constraints, and alternatives. Add `kind/design` when that label exists.
2. Copy [the template](0000-template.md) to the next available four-digit
   number and use a short descriptive name.
3. Open a pull request with status `Proposed` and link the design issue.
4. Collect implementation, operator, and upstream feedback. Keep the proposal
   open for at least five working days unless it only corrects an obvious
   mistake.
5. An approver changes the status to `Accepted` when the decision has
   consensus and its safety, migration, and evidence requirements are clear.
6. Merge implementation separately. Acceptance authorizes the documented
   direction; it does not prove that the implementation or release is ready.

Use one of these statuses:

- `Proposed` while the decision is under review
- `Accepted` after project approval
- `Rejected` when the proposal will not be pursued
- `Superseded` when a later ADR replaces it

Do not edit the decision of an accepted ADR to make history look current. Add a
new ADR and link both records.

## Index

| ADR | Status | Decision |
| --- | --- | --- |
| None | — | The project has not accepted an ADR yet |
