# Governance

gateway-api-openstack is currently an independent open source project. It is
not a Kubernetes project or a subproject of any Kubernetes SIG. It is not an
OpenInfra or cloud-provider-openstack subproject. This file describes how the
repository is run while that remains true.

## Principles

Project work is discussed and reviewed in public unless it concerns an
unreleased security issue or a private code of conduct report. Decisions should
start with the user or operator problem and should include the ownership,
failure, upgrade, and test consequences.

Recorded evidence has more weight than a compatibility assumption. A current
maintainer title does not make one person's OpenStack environment representative
of every cloud.

## Roles

The `OWNERS` file is the source of reviewer and approver assignments.

Contributors file issues, test changes, write documentation, review proposals,
or submit pull requests. A contributor does not need a formal role.

Reviewers have shown sustained knowledge of part of the repository. They review
changes for correctness, maintainability, and test coverage. Reviewers are not
automatically able to accept a change on behalf of the project.

Approvers are the maintainers of this repository. They are responsible for the
project boundary, architecture decisions, releases, security response, and the
long-term health of the code they approve. Approval is a responsibility, not a
reward for commit count.

The project currently has one reviewer and approver. That is a project risk and
not a community claim. The roadmap tracks independent review, release coverage,
and maintainers from more than one organization as community maturity work.

## Decisions

Most changes are decided in a pull request after the relevant maintainers and
contributors have had a reasonable chance to review it. Small, reversible
changes do not need a separate proposal.

A public design issue and an ADR are required for changes listed in
[the ADR process](docs/design/adr/README.md). Proposed ADRs should normally be
open for at least five working days. A longer review is appropriate for a new
API, an ownership boundary, or a change that is difficult to reverse.

The project prefers consensus. If reviewers disagree, the author should record
the alternatives and the unresolved concern instead of merging around it. The
approvers make the final repository decision after the public discussion. While
there is only one approver, that person must record the reason for a material
decision and any dissent in the issue or ADR.

Accepted ADRs override older proposed design text. A later change is made with
a new ADR that supersedes the old one; accepted history is not rewritten.

## Adding and removing maintainers

A reviewer should have a useful history of contributions and reviews in the
area they will own. An approver should already be an active reviewer, understand
the safety and compatibility contracts, and be willing to handle releases and
project operations.

Role changes use a public pull request to `OWNERS`. Existing approvers decide by
consensus after asking active contributors for feedback. The pull request should
point to the work that demonstrates the candidate's responsibility. Employer
or organization diversity is considered because the project should not depend
on one company or one person.

A maintainer may step down at any time. An inactive maintainer should be moved
out of an active role after a public attempt to contact them and a reasonable
handoff period. Removal does not erase authorship or prior contributions.

## Releases and security

The release process is documented in [docs/releasing.md](docs/releasing.md).
Every release has a public release issue and immutable artifacts. When the
project has two approvers, the release owner must not be the only approver of
their release.

Security reports follow [SECURITY.md](SECURITY.md). Security work may be private
until a coordinated disclosure, but the resulting fix, affected versions, and
credit should be made public when disclosure is safe.

## A future community home

An upstream implementation listing and a repository home are different things.
The project can submit honest Gateway API conformance results while it remains
independent. Moving the repository into a Kubernetes or OpenInfra organization
would require that community's public process and approval.

The project will not describe such a move as planned or endorsed until that
decision exists. Before asking for it, the repository should have sustained
adoption, maintainers from more than one organization, independent design and
release review, a working security handoff, and regular participation in the
relevant upstream communities.

## Changes to this document

Governance changes use the same public review process as an ADR. Keep this file
aligned with actual practice. Do not add roles, meetings, or communication
channels that do not exist.
