# Amphora compatibility evidence

The controller accepts only Amphora.
OVN and vendor providers are out of scope.
This page records the Amphora environments that have actually been tested.
It does not claim support beyond that evidence.

No controller release has a supported environment profile yet.
The Phase 0 probe tested individual Octavia and Neutron operations in one Amphora environment, but its exact versions and topology have not been published.
It did not test controller traffic, recovery, deletion, or Gateway API conformance.

The repository includes an [OpenStack E2E test guide](../testing-openstack-e2e.md) and a report template.
Their presence does not change the current evidence level.
This page changes only after a report contains results from a named controller revision and environment.

The project uses `probed`, `verified`, `supported`, and `conformant` as separate evidence levels.
The definitions are in the roadmap.
A higher level is never inferred from a lower one.

## Evidence matrix

| Capability | Current evidence | Evidence required for a support claim | Known constraints |
| --- | --- | --- | --- |
| Load balancer and HTTP listener lifecycle | Phase 0 resource probe passed. Adapter tests cover listener recreation and administrative state repair | Initial creation, converged no-op, restart, drift, and E2E deletion with full ownership checks | One Amphora load balancer owned by each Gateway |
| L7 policies and rules | Phase 0 resource probe passed. Constrained controller unit tests exist | Real traffic, precedence, recovery after partial creation, and pinned conformance results | Only Gateway API semantics that Octavia represents exactly |
| NodePort members and health monitors | Resource creation passed. Controller traffic E2E pending | Traffic for `externalTrafficPolicy: Cluster` and `Local`, endpoint churn, Node lifecycle, and leak checks | Worker addresses and NodePorts must be reachable from Amphora |
| Floating IP allocation | Phase 0 probe and adapter ownership tests passed | E2E tests with and without a Floating IP, quota failure, detach recovery, and cleanup | Only Floating IPs created by the controller. Existing addresses are not adopted |
| Reconciliation failure recovery | Unit and adapter tests cover typed failures, safe status and Events, jittered retries, periodic resync, shared client throttling, selected resource recreation, semantic status no-ops, finalizer retention, and the read-only ownership audit. API server envtest covers stale binding conflicts, finalizer checkpoints, progressing cleanup, and finalization with a fresh reconciler | Restart, leader change, `PENDING_*`, timeout, quota, rate limit, external deletion, fault injection after partial creation, and the blocked finalization workflow in OpenStack | Ownership conflict stops mutation and uses a slower retry interval |
| Gateway API bundle handling | Unit tests cover exact, missing, and mixed CRD bundle versions, event filtering, mutation blocking, and cleanup during a mismatch. API server envtest loads the pinned Standard Channel CRDs and exercises the status subresource | Upgrade testing with the pinned Standard Channel manifests | This release accepts only the v1.6.1 bundle and does not advertise Gateway or HTTPRoute conformance |
| Network and backend security automation | Not implemented | A documented network selection procedure and repeatable end-to-end tests for `Referenced`, `Unmanaged`, and any approved `Managed` mode | The controller does not own networks, subnets, routers, foreign security groups, or worker ports |
| Terminated HTTPS and Barbican | Not implemented | Tests for authorized Secret references, certificate creation and rotation, restart recovery, deletion, SNI, and orphan cleanup | Requires Barbican and a certificate lifecycle that verifies ownership before mutation or deletion |
| Pod IP members | Not implemented | Tests that cover CNI routability, EndpointSlice lifecycle, draining, security, and source IP behavior | If added, this mode will remain experimental and require explicit opt-in. NodePort remains the default mode with the broadest compatibility |
| Gateway API conformance | No report submitted | Unmodified report for the pinned Gateway API version with every claimed feature passing | Do not claim a conformance profile unless all of its Core features pass |

## Environment profile

Use the [OpenStack E2E report template](../reports/openstack-e2e-template.md) as the environment profile.
It records the exact controller artifact, cluster and cloud versions, literal Amphora provider and topology, network path, endpoint behavior, optional services, quotas, tests, and redacted evidence.
A compatibility statement applies only to that recorded release and environment.
“Works on OpenStack” is not sufficient evidence.

## Reporting an environment

Use the OpenStack environment report issue template.
A report must confirm Amphora and explain how Amphora reaches backend members.
Publish only information you are authorized to share.
Remove credentials, tokens, tenant identifiers, private addresses, customer names, and other sensitive topology details.

Prefer a disposable, dedicated OpenStack project for controller checks and release evidence.
The E2E runner accepts either a dedicated or shared project and applies the same run-scoped controller, exact project and network checks, and ownership audit to both.
Shared mode is not an OpenStack authorization boundary.
A report for a shared project must name the mode and include a reviewed, attributed project-wide inventory difference.

Pin the controller image by digest and install the Gateway API v1.6.1 Standard Channel CRDs.
Follow the [E2E test guide](../testing-openstack-e2e.md) for the preflight, baseline scenarios, shared project limits, fault result semantics, cleanup order, and leak inventory.

Use the [OpenStack E2E report template](../reports/openstack-e2e-template.md) for evidence kept in the repository.
Use the [conformance gap template](../reports/conformance-gap-template.md) for a local gap analysis.
Neither template is evidence until it contains actual results from the named revision and environment.
