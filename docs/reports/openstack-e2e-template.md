# OpenStack controller E2E report: environment name

Status: Draft

Date: YYYY-MM-DD

Controller release or commit:

Controller image digest:

Result owner:

## Environment

Record the Kubernetes and Gateway API versions, CNI, kube-proxy mode or
replacement, IP family, and relevant Service and Pod network behavior. Record
the OpenStack and Octavia releases, negotiated API microversions, literal
provider identifier, Amphora topology, optional Barbican availability, and
relevant quotas.

Describe how the VIP, member, and external networks relate and how Amphora
reaches the selected Node addresses and NodePorts. Remove resource IDs,
credentials, private addresses, and customer details.

## Installation

Record the exact manifests, flags, controller name, immutable image digest, and
Gateway API CRD bundle used. State whether the test started from a clean
project or an upgrade.

## Results

| Test | Result | Evidence or notes |
| --- | --- | --- |
| GatewayClass, Gateway, and HTTPRoute status | Not run | |
| HTTP traffic through the published address | Not run | |
| Converged reconcile makes zero OpenStack mutations | Not run | |
| Restart after a partial transition creates no duplicate | Not run | |
| Restart after convergence creates no duplicate | Not run | |
| `externalTrafficPolicy: Cluster` member changes | Not run | |
| `externalTrafficPolicy: Local` endpoint and Node changes | Not run | |
| Owned administrative state drift repair | Not run | |
| External deletion of an owned child resource | Not run | |
| Quota, timeout, and Octavia failure handling | Not run | |
| HTTPRoute and Gateway finalization | Not run | |
| Ownership audit during blocked finalization | Not run | |
| Post-test leak inventory | Not run | |

For every pass, link a redacted command log or describe how the assertion was
made. Record every skip and failure. A retry that hides an unexplained failure
is not a pass.

## API calls and timing

Record load balancer creation time, route update time, mutation counts for a
converged reconcile, and API calls during the endpoint and Node change tests.
Note the Amphora and quota cost for the tested topology.

## Findings

List controller defects, environment failures, unsupported behavior, and
follow-up issues. State what this report verifies and what it does not verify.

## Redaction review

- [ ] No credentials, tokens, certificates, or private API bodies are present.
- [ ] Project IDs, resource IDs, private addresses, and customer names are
      removed.
- [ ] Linked evidence is expected to remain available.
