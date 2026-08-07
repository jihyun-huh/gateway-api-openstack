# OpenStack provider compatibility

No provider has a supported controller release yet. The retained Phase 0 probe
has passed the following resource-level checks in an initial Amphora test
environment, but the environment versions and topology still need to be
published before these results count as a reproducible compatibility report.

| Capability | Amphora | OVN | Other providers |
| --- | --- | --- | --- |
| Load balancer lifecycle | Phase 0 probe passed; report pending | Unverified | Unverified |
| HTTP listener | Phase 0 probe passed; report pending | Unverified | Unverified |
| L7 policies and rules | Phase 0 probe passed; report pending | Expected limitation; verify | Unverified |
| NodePort members | Resource creation passed; traffic/controller E2E pending | Unverified | Unverified |
| Pod IP members | Environment-specific | Environment-specific | Environment-specific |
| Health monitors | Phase 0 probe passed; report pending | Unverified | Unverified |
| Floating IP | Phase 0 probe passed; report pending | Unverified | Unverified |
| TLS termination | Unverified | Unverified | Unverified |
| Barbican integration | Unverified | Unverified | Unverified |
| Resource identity tags | Phase 0 probe passed; report pending | Unverified | Unverified |

## Reporting an environment

Use the OpenStack environment report issue template. Include versions,
topology, and test evidence, but remove credentials and private tenant details.

“Works on OpenStack” is not a sufficient compatibility statement. Reports must
name the Octavia provider and the member reachability mode.
