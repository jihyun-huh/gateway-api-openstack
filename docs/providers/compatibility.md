# OpenStack provider compatibility

No provider is supported yet. This table records validation status and must be
updated from reproducible tests.

| Capability | Amphora | OVN | Other providers |
| --- | --- | --- | --- |
| Load balancer lifecycle | Unverified | Unverified | Unverified |
| HTTP listener | Unverified | Unverified | Unverified |
| L7 policies and rules | Unverified | Expected limitation; verify | Unverified |
| NodePort members | Unverified | Unverified | Unverified |
| Pod IP members | Environment-specific | Environment-specific | Environment-specific |
| Health monitors | Unverified | Unverified | Unverified |
| Floating IP | Unverified | Unverified | Unverified |
| TLS termination | Unverified | Unverified | Unverified |
| Barbican integration | Unverified | Unverified | Unverified |
| Resource identity tags | Unverified | Unverified | Unverified |

## Reporting an environment

Use the OpenStack environment report issue template. Include versions,
topology, and test evidence, but remove credentials and private tenant details.

“Works on OpenStack” is not a sufficient compatibility statement. Reports must
name the Octavia provider and the member reachability mode.
