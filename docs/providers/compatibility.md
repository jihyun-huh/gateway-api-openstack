# Amphora compatibility evidence

The controller accepts only Amphora.
OVN and vendor providers are out of scope.
This page records the Amphora environments that have actually been tested.
It does not claim support beyond that evidence.

No controller release has a supported environment profile yet.
The Phase 0 probe tested individual Octavia and Neutron operations in one Amphora environment, but its exact versions and topology have not been published.
It did not test controller traffic, recovery, deletion, or Gateway API conformance.

Update this matrix when a reviewed public report supports a compatibility claim for a named controller revision and environment.
For routine development checks, follow the [test evidence policy](../reports/README.md).

The project uses `probed`, `verified`, `supported`, and `conformant` as separate evidence levels.
The definitions are in the roadmap.
A higher level is never inferred from a lower one.

## Development validation

On 2026-10-09, a manual OpenStack E2E run passed all 12 required foundation checks against controller revision `221d9ba` with local test harness changes.
The run used selected NodePort backends with `externalTrafficPolicy: Local`, a private VIP, and no Floating IP.
It covered HTTP traffic, Gateway API status, leader recovery, a cold controller restart, converged metrics, deletion, and finalizer completion.
Ownership audits passed, cleanup returned to baseline, and the shared project's resource identities and compared settings were unchanged.

Five fault checks were `Not run`: external deletion of an owned child, blocked finalization, quota failure, request timeout and rate limiting, and Octavia resource failure.
A separate VIP diagnostic request timed out.
Its timing was not synchronized with teardown, so the cause remains unresolved.

Dependencies and the test harness changed after this run, so it does not establish live validation of the current revision.
Detailed records remain local under the [test evidence policy](../reports/README.md).
This development result does not establish a supported environment profile or Gateway API conformance.

## Evidence matrix

| Capability | Current evidence | Evidence required for a support claim | Known constraints |
| --- | --- | --- | --- |
| Load balancer and HTTP listener lifecycle | Phase 0 resource probe and [development E2E lifecycle checks](#development-validation) passed. Adapter tests cover listener recreation and administrative state repair | Initial creation, converged no-op, restart, drift, and E2E deletion with full ownership checks | One Amphora load balancer owned by each Gateway |
| L7 policies and rules | Phase 0 resource probe and [development E2E HTTP traffic](#development-validation) passed. Constrained controller unit tests exist | Real traffic, precedence, recovery after partial creation, and pinned conformance results | Only Gateway API semantics that Octavia represents exactly |
| NodePort members and health monitors | Phase 0 resource creation and [development E2E with Local NodePort backends](#development-validation) passed | Traffic for `externalTrafficPolicy: Cluster` and `Local`, endpoint churn, Node lifecycle, and leak checks | Selected Node addresses and NodePorts must be reachable from Amphora |
| Floating IP allocation | Phase 0 probe and adapter ownership tests passed | E2E tests with and without a Floating IP, quota failure, detach recovery, and cleanup | Only Floating IPs created by the controller. Existing addresses are not adopted |
| Reconciliation failure recovery | [Development E2E leader and cold restart recovery](#development-validation) passed. Unit and adapter tests cover typed failures, safe status and Events, jittered retries, periodic resync, shared client throttling, selected resource recreation, semantic status no-ops, finalizer retention, and the read-only ownership audit. API server envtest covers stale binding conflicts, finalizer checkpoints, progressing cleanup, and finalization with a fresh reconciler | Restart, leader change, `PENDING_*`, timeout, quota, rate limit, external deletion, fault injection after partial creation, and the blocked finalization workflow in OpenStack | Ownership conflict stops mutation and uses a slower retry interval |
| Gateway API bundle handling | Unit tests cover exact, missing, and mixed CRD bundle versions, event filtering, mutation blocking, and cleanup during a mismatch. API server envtest loads the pinned Standard Channel CRDs and exercises the status subresource | Upgrade testing with the pinned Standard Channel manifests | This release accepts only the v1.6.1 bundle and does not advertise Gateway or HTTPRoute conformance |
| Network and backend security automation | Not implemented | A documented network selection procedure and repeatable end-to-end tests for `Referenced`, `Unmanaged`, and any approved `Managed` mode | The controller does not own networks, subnets, routers, foreign security groups, or worker ports |
| Terminated HTTPS and Barbican | Not implemented | Tests for authorized Secret references, certificate creation and rotation, restart recovery, deletion, SNI, and orphan cleanup | Requires Barbican and a certificate lifecycle that verifies ownership before mutation or deletion |
| Pod IP members | Not implemented | Tests that cover CNI routability, EndpointSlice lifecycle, draining, security, and source IP behavior | If added, this mode will remain experimental and require explicit opt-in. NodePort remains the default mode with the broadest compatibility |
| Gateway API conformance | No report submitted | Unmodified report for the pinned Gateway API version with every claimed feature passing | Do not claim a conformance profile unless all of its Core features pass |

## Environment profile

A compatibility report must identify the controller artifact, relevant versions, Amphora topology, network path, tested scope, and accessible supporting evidence.
Mark unknown properties explicitly.
A compatibility statement applies only to that recorded release and environment.
“Works on OpenStack” is not sufficient evidence.

## Reporting an environment

Use the OpenStack environment report issue template.
A report must confirm Amphora and explain how Amphora reaches backend members.
Publish only information you are authorized to share.
Remove credentials, tokens, tenant identifiers, private addresses, customer names, and other sensitive topology details.

Follow the [E2E test guide](../testing-openstack-e2e.md) for prerequisites, project isolation, test scope, cleanup, and inventory checks.
Keep tenant inventories private and summarize the cleanup outcome and any limitations under the [test evidence policy](../reports/README.md).
Use the [conformance gap template](../reports/conformance-gap-template.md) for a local gap analysis.
