# Amphora compatibility evidence

The controller accepts only Amphora under the gateway-api-openstack product
contract. OVN and vendor providers are out of scope rather than untested
alternatives. Release support remains specific to the evidence below.

No controller release has a supported environment profile yet. The retained
Phase 0 probe exercised individual Octavia and Neutron operations in one
Amphora environment, but its exact versions and topology have not been
published. It does not demonstrate controller traffic, recovery, deletion, or
Gateway API conformance.

## Evidence matrix

| Capability | Current evidence | Evidence required for a support claim | Known constraints |
| --- | --- | --- | --- |
| Load balancer and HTTP listener lifecycle | Phase 0 resource probe passed; reproducible report pending | Controller create, converged no-op, restart, drift, and identity-safe deletion E2E | One controller-owned Amphora load balancer per Gateway |
| L7 policies and rules | Phase 0 resource probe passed; constrained controller unit tests exist | Real traffic, precedence, partial-create recovery, and pinned conformance results | Only Gateway API semantics that Octavia represents exactly |
| NodePort members and health monitors | Resource creation passed; controller traffic E2E pending | Traffic for `externalTrafficPolicy: Cluster` and `Local`, endpoint churn, Node lifecycle, and leak checks | Worker addresses and NodePorts must be reachable from Amphora |
| Floating IP allocation | Phase 0 probe and identity-safe adapter tests passed | Floating IP and VIP-only path E2E, quota failure, detach recovery, and cleanup | Only controller-created Floating IPs; existing addresses are not adopted |
| Reconciliation failure recovery | Unit coverage exists for selected ownership and deletion cases | Restart, leader change, `PENDING_*`, timeout, quota, rate limit, external deletion, and partial graph fault injection | Ownership conflict stops mutation |
| Network and backend security automation | Not implemented | Deterministic resolution plus `Referenced`, `Unmanaged`, and any approved `Managed` mode E2E | Networks, subnets, routers, foreign security groups, and worker ports are not controller-owned |
| Terminated HTTPS and Barbican | Not implemented | Secret authorization, certificate create/rotation/restart/delete, SNI, and orphan checks | Requires Barbican and identity-safe secret/container lifecycle |
| Pod IP members | Not implemented | CNI-specific routability, EndpointSlice lifecycle, drain, security, and source-IP evidence | Experimental opt-in only; NodePort remains the compatibility path |
| Gateway API conformance | No report submitted | Unmodified report for the pinned Gateway API version with every claimed feature passing | Only partial conformance can be reported when Amphora cannot represent Core semantics |

## Environment profile

Every published compatibility result must include:

- controller release or commit and Gateway API version;
- Kubernetes version, CNI, kube-proxy mode, and Service and Pod CIDRs;
- OpenStack and Octavia releases and negotiated API microversions;
- confirmation that the Octavia provider is Amphora and whether its topology is
  single or active-standby;
- VIP, member, and external network relationships, with private identifiers
  removed;
- Node address selection and backend member mode;
- Floating IP, custom VIP security group, and Barbican availability;
- the least-privilege roles and relevant quotas;
- exact tests for traffic, status, restart, drift, deletion, leaks, and
  conformance; and
- links to redacted logs or reports that another operator can reproduce.

A compatibility statement applies only to the recorded release and environment
profile. “Works on OpenStack” is not sufficient evidence.

## Reporting an environment

Use the OpenStack environment report issue template. Reports for the supported
path must confirm Amphora and name the member reachability model. Share only
information you are authorized to publish; remove credentials, tokens, tenant
identifiers, private addresses, customer names, and other sensitive topology
details.
