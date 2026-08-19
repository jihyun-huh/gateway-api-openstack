# ADR 0000: Short decision title

Status: Proposed

Date: YYYY-MM-DD

Owners: GitHub usernames

Review issue: Link to the public design issue

## Context

Describe the concrete user or operator problem, current behavior, and the
constraints that make a decision necessary. State what is out of scope.

## Decision

State the decision in direct terms. Include the ownership boundary, immutable
identity, API shape, or reconciliation rule that future code must preserve.

## Consequences

Describe the useful result and the costs. Include compatibility limits,
operational work, new failure modes, and anything that becomes harder to change.

## Alternatives considered

List the serious alternatives and why they were not selected. Do not include
options that nobody considered viable.

## Safety and recovery

Explain mutation authority, crash and restart behavior, duplicate discovery,
deletion order, and what the controller does when ownership cannot be proved.

## API and upgrade impact

Describe versioning, defaulting, migration, rollback, and cleanup after an
upgrade. Note any effect on Gateway API status or conformance.

## Test and evidence plan

List the unit, adapter, envtest, OpenStack, upgrade, fault, and conformance
evidence needed before the behavior can be released.

## Follow-up work

List implementation or documentation work that is intentionally outside this
decision. Link later ADRs instead of expanding this one after acceptance.
