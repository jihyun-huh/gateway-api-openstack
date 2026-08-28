# OpenStack controller E2E report: environment name

Status: Draft template.
No test result is implied.

Date: YYYY-MM-DD

Controller source revision (full Git commit):

Controller release, if any:

Controller image digest:

Backend `agnhost` image and digest:

Result owner:

OpenStack project mode (`dedicated` or `shared`):

Project isolation or shared-project risk review confirmed by:

## Environment

Record the Kubernetes and Gateway API versions, CNI, kube-proxy mode or replacement, IP family, and relevant Service and Pod network behavior.
Record the OpenStack and Octavia releases, negotiated API microversions, literal provider identifier, Amphora topology, optional Barbican availability, and relevant quotas.

Describe how the VIP, member, and external networks relate and how Amphora reaches the selected Node addresses and NodePorts.
Remove resource IDs, credentials, private addresses, and customer details.

For a shared project, record who approved the run, how the Kubernetes resources and controller installation were separated from other users, and who reviewed the redacted project-wide inventory difference.
Do not include the expected project, subnet, or network IDs.

## Installation

Record the exact manifests, flags, controller name, immutable controller and backend image digests, and Gateway API v1.6.1 Standard Channel CRD bundle used.
State whether the test started from a clean project or an upgrade.
Confirm that both image references contain `@sha256:`.
For `dedicated` mode, confirm that the project was disposable, dedicated to this run, and empty of unrelated resources.
For `shared` mode, confirm that the exact controller name and cluster ID audit scope was empty and that the controller Namespace and RBAC objects were unique to the run.
Also confirm that the controller and audit credentials independently authenticated to the expected project and that the Pod template was bound to the final immutable ConfigMap and Secret versions.
Do not copy the expected project ID or source annotation values into the report.

Follow the [OpenStack E2E test guide](../testing-openstack-e2e.md).
Record the actual harness target and its non-secret settings.
Do not copy a populated credential file or kubeconfig into this report.

## Result meanings

- `Passed` means the named assertion was observed and retained evidence is linked or described.
- `Failed` means the scenario ran and the expected behavior was not observed.
- `Skipped` means an optional check was outside the configured run.
  It is not evidence for that check.
- `Not run` means the scenario was not attempted or did not reach the point where the assertion could be evaluated.

Do not replace `Not run` with a pass based on unit, adapter, envtest, or Phase 0 probe coverage.
A cloud or cluster failure that prevents the assertion belongs in the notes and is not a controller pass.

## Results

The following rows are the required Phase 2 foundations checks.

| Test | Result | Evidence or notes |
| --- | --- | --- |
| Preflight validates the explicit context, selected project mode, controller identity, immutable configuration sources, pinned bundle, independent project authentication, and empty exact-scope audit baseline | Not run | |
| Run-scoped NodePort backend becomes ready | Not run | |
| GatewayClass status | Not run | |
| Gateway status | Not run | |
| HTTPRoute status | Not run | |
| Active ownership inventory matches the two controller bindings | Not run | |
| HTTP traffic through the published address | Not run | |
| Leader Pod deletion restores the same active ownership inventory | Not run | |
| Cold controller restart restores the same active ownership inventory | Not run | |
| Current-leader metrics window has a fresh observation and no OpenStack mutation | Not run | |
| HTTPRoute and Gateway finalization completes in dependency order | Not run | |
| Post-test scoped ownership audit returns to its empty baseline | Not run | |
| Independent redacted project-wide inventory returns to baseline, or every shared-project difference is attributed | Not run | |

The following rows need their own approved setup or fault mechanism.
They are not implied by a foundations run.
Do not run fault, quota, or destructive recovery cases in a shared project.

| Test | Result | Evidence or notes |
| --- | --- | --- |
| Restart at an approved partial-transition checkpoint leaves no duplicate in an independent provider inventory | Not run | |
| `externalTrafficPolicy: Cluster` member changes | Not run | |
| `externalTrafficPolicy: Local` endpoint and Node changes | Not run | |
| Owned administrative state drift repair | Not run | |
| External deletion of an owned child resource | Not run | |
| Quota, timeout, and Octavia failure handling | Not run | |
| Ownership audit during blocked finalization | Not run | |

For every pass, link a redacted command log or describe how the assertion was made.
Record every skip and failure.
A retry that hides an unexplained failure is not a pass.

The scoped ownership audit rows cover only resources that remain visible to the audit at each snapshot.
They do not prove a complete desired graph, rule out a transient duplicate, or replace the independent project-wide inventory.
See the [ownership audit contract](../design/ownership-audit.md) for the known limits.

In shared mode, a pass shows that the automated run stayed within its exact logical identity as far as these observations can tell.
It does not show that the credential could not access unrelated resources, that shared quota and API capacity were unaffected, or that no detached resource lost all scope metadata.
Record the reviewed and attributed project-wide inventory difference before making a leak claim.

## API calls and timing

Record load balancer creation time, route update time, mutation counts for a converged reconcile, and API calls during the endpoint and Node change tests.
Note the Amphora and quota cost for the tested topology.

## Findings

List controller defects, environment failures, unsupported behavior, and follow-up issues.
State what this report verifies and what it does not verify.

## Redaction review

- [ ] No credentials, tokens, certificates, or private API bodies are present.
- [ ] No `clouds.yaml`, kubeconfig, populated Secret, or unredacted environment file is present.
- [ ] Project IDs, resource IDs, private addresses, and customer names are removed.
- [ ] ConfigMap and Secret UIDs, resourceVersions, and source annotation values are removed.
- [ ] Pod names and UIDs and leader Lease holder identities are removed from proxy URLs, logs, and linked artifacts.
- [ ] The controller and backend images are recorded by immutable digest and the controller source revision is recorded.
- [ ] The Gateway API CRD bundle is the v1.6.1 Standard Channel bundle.
- [ ] The project mode and its isolation or shared-project risk review are recorded without private identifiers.
- [ ] Linked evidence is expected to remain available.
