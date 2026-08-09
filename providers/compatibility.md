# Amphora compatibility evidence

The controller accepts only Amphora. OVN and vendor providers are out of scope.
This page records the Amphora environments that have actually been tested. It
does not claim support beyond that evidence.

No controller release has a supported environment profile yet. The Phase 0
probe tested individual Octavia and Neutron operations in one Amphora
environment, but its exact versions and topology have not been published.
It did not test controller traffic, recovery, deletion, or Gateway API
conformance.

## Evidence matrix

| Capability | Current evidence | Evidence required for a support claim | Known constraints |
| --- | --- | --- | --- |
| Load balancer and HTTP listener lifecycle | Phase 0 resource probe passed. Reproducible report pending | Initial creation, converged no-op, restart, drift, and E2E deletion with full ownership checks | One Amphora load balancer owned by each Gateway |
| L7 policies and rules | Phase 0 resource probe passed. Constrained controller unit tests exist | Real traffic, precedence, recovery after partial creation, and pinned conformance results | Only Gateway API semantics that Octavia represents exactly |
| NodePort members and health monitors | Resource creation passed. Controller traffic E2E pending | Traffic for `externalTrafficPolicy: Cluster` and `Local`, endpoint churn, Node lifecycle, and leak checks | Worker addresses and NodePorts must be reachable from Amphora |
| Floating IP allocation | Phase 0 probe and adapter ownership tests passed | E2E tests with and without a Floating IP, quota failure, detach recovery, and cleanup | Only Floating IPs created by the controller. Existing addresses are not adopted |
| Reconciliation failure recovery | Unit coverage exists for selected ownership and deletion cases | Restart, leader change, `PENDING_*`, timeout, quota, rate limit, external deletion, and fault injection after partial creation | Ownership conflict stops mutation |
| Network and backend security automation | Not implemented | A documented network selection procedure and repeatable end-to-end tests for `Referenced`, `Unmanaged`, and any approved `Managed` mode | The controller does not own networks, subnets, routers, foreign security groups, or worker ports |
| Terminated HTTPS and Barbican | Not implemented | Tests for authorized Secret references, certificate creation and rotation, restart recovery, deletion, SNI, and orphan cleanup | Requires Barbican and a certificate lifecycle that verifies ownership before mutation or deletion |
| Pod IP members | Not implemented | Tests that cover CNI routability, EndpointSlice lifecycle, draining, security, and source IP behavior | If added, this mode will remain experimental and require explicit opt-in. NodePort remains the default mode with the broadest compatibility |
| Gateway API conformance | No report submitted | Unmodified report for the pinned Gateway API version with every claimed feature passing | Do not claim a conformance profile unless all of its Core features pass |

## Environment profile

Every compatibility report must record:

- controller release or commit and Gateway API version
- Kubernetes version, CNI, kube-proxy mode, and Service and Pod CIDRs
- OpenStack and Octavia releases and negotiated API microversions
- confirmation that the Octavia provider is Amphora and whether its topology is
  `SINGLE` or `ACTIVE_STANDBY`
- VIP, member, and external network relationships, with private identifiers
  removed
- Node address selection and backend member mode
- Floating IP, custom VIP security group, and Barbican availability
- the minimum required roles and relevant quotas
- tests run for traffic, status, restart, drift, deletion, leaks, and
  conformance
- links to redacted logs and reports, with enough detail for another operator
  to reproduce the tests

A compatibility statement applies only to the recorded release and environment
profile. “Works on OpenStack” is not sufficient evidence.

## Reporting an environment

Use the OpenStack environment report issue template. A report must confirm
Amphora and explain how Amphora reaches backend members. Publish only
information you are authorized to share. Remove credentials, tokens, tenant
identifiers, private addresses, customer names, and other sensitive topology
details.
