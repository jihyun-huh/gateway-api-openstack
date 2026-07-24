# Architecture

Status: proposed  
Audience: contributors, reviewers, OpenStack operators, Gateway API implementers

## Problem statement

Kubernetes clusters on OpenStack need a Gateway API implementation that can
provision Octavia resources directly without depending on the lifecycle or
release priorities of the cloud-provider-openstack Service controller.

The implementation must coexist with cloud-provider-openstack and must not
claim features that the selected Octavia provider cannot supply.

## Design principles

1. **Gateway API is the contract.** Standard resources, status conditions, and
   conformance tests drive behavior.
2. **Ownership is exclusive.** No OpenStack resource may be reconciled by both
   this project and OCCM.
3. **Provider capability is explicit.** Unsupported behavior is reported, never
   silently approximated.
4. **Reconciliation is idempotent.** Restart, retry, and external drift are
   normal controller conditions.
5. **Deletion is identity-safe.** A name match is never sufficient to delete an
   OpenStack resource.
6. **OpenStack access uses Gophercloud.** Controller code must not shell out to
   the OpenStack CLI.
7. **Proxy profiles are separate.** Traefik or Envoy deployment configuration
   is not part of the native reconciliation loop.

## Ownership contract

### cloud-provider-openstack owns

- cloud node lifecycle and other CCM responsibilities;
- Kubernetes `Service type=LoadBalancer`;
- Octavia resources created for those Services.

### openstack-gateway-controller owns

- Gateway API resources whose `GatewayClass.spec.controllerName` exactly
  matches the project's configured controller name;
- Octavia and Neutron resources created for those Gateways;
- any helper resources it explicitly creates and labels with a Gateway UID.

### openstack-gateway-controller may read

- `Service`;
- `EndpointSlice`;
- `Secret`;
- `ReferenceGrant`;
- `Node`, if the selected member mode requires Node addresses;
- provider configuration and credentials.

Reading a backend Service does not imply ownership of that Service.

### openstack-gateway-controller must never

- watch `Service type=LoadBalancer` as a provisioning source;
- add or remove OCCM finalizers;
- adopt an Octavia resource using its name alone;
- mutate or delete a resource whose full project identity does not match;
- create a `Service type=LoadBalancer` for its native path;
- install a proxy controller inside the native reconciliation loop.

## Proposed resource mapping

| Kubernetes resource | OpenStack resource | Notes |
| --- | --- | --- |
| GatewayClass | Provider/class configuration | Reconciled only for the exact controller name |
| Gateway | Octavia Load Balancer | One load balancer per Gateway initially |
| Gateway address | Neutron Floating IP or VIP | Status must contain the client-reachable address |
| Gateway listener | Octavia Listener | HTTP first; terminated HTTPS later |
| HTTPRoute parent attachment | Listener association and status | Must follow Gateway API attachment rules |
| HTTPRoute rule | L7 Policy | Only representable matches and actions |
| HTTPRoute match | L7 Rule | Host/path first |
| backendRef Service | Octavia Pool | One backend per rule in the MVP |
| Service endpoints or nodes | Octavia Members | Depends on member mode |
| health policy | Octavia Health Monitor | Defaults must be documented |
| TLS Secret | Barbican Secret/Container | Phase 2 |

## Resource identity

Every created OpenStack resource should carry, where supported:

- project identifier;
- controller identifier;
- cluster identifier;
- Kubernetes namespace and name;
- immutable Gateway UID;
- Kubernetes resource role;
- controller release version.

Deletion requires all immutable identity fields to match. If an API does not
support tags, the design must define an equivalent safe identity mechanism
before that resource type is managed.

The controller should also retain the OpenStack resource IDs in Kubernetes
annotations or internal state only when doing so does not make those values the
sole source of truth. Recovery must be possible by listing resources with
project identity.

## Reconciliation boundaries

### GatewayClass reconciler

- filters on the exact controller name;
- validates class configuration;
- discovers or validates provider capabilities;
- writes `Accepted` and `supportedFeatures`;
- prevents deletion while Gateways use the class.

### Gateway reconciler

- validates listener support;
- ensures the load balancer and address;
- ensures listeners;
- aggregates attached-route status;
- writes Gateway and listener conditions;
- performs identity-safe finalization.

### HTTPRoute reconciler

- evaluates parent attachment;
- resolves references and ReferenceGrant rules;
- validates supported matches and filters;
- builds L7 policies, rules, pools, and members;
- writes parent status;
- watches relevant backend changes.

## Backend member modes

Backend reachability is an architecture decision, not a hidden installation
detail.

### NodePort mode

Octavia members point to worker Node addresses and a NodePort.

Advantages:

- works when Pod CIDRs are not routable from Octavia;
- resembles the proven model used by the existing Octavia Ingress Controller.

Risks:

- ordinary ClusterIP backend Services do not expose a NodePort;
- generated helper Services need strict ownership and EndpointSlice behavior;
- health behavior depends on kube-proxy and traffic policy.

### Pod IP mode

Octavia members point directly to EndpointSlice addresses.

Advantages:

- avoids an extra node hop;
- naturally follows endpoint readiness.

Risks:

- requires routable Pod networking;
- behavior varies by CNI and OpenStack topology;
- security groups and routes may need additional management.

The MVP mode will be selected by ADR after both are tested in Openstack Cluster. The
controller must expose the selected mode in class configuration and must not
guess.

## Provider capability model

The controller needs a capability object resolved from:

- selected Octavia provider;
- OpenStack service versions;
- verified project configuration;
- capabilities proven by project tests.

Capabilities influence:

- listener protocols;
- L7 matches and actions;
- TLS termination;
- health monitors;
- tags;
- allowed Gateway API `supportedFeatures`.

Unknown capability is treated as unsupported until verified. The controller
must use standard Gateway API conditions and clear messages when rejecting an
unsupported resource.

## Configuration layers

1. **Controller configuration:** credentials, region, cluster identity,
   metrics, logging, and safe defaults.
2. **GatewayClass parameters:** provider, networks, subnets, member mode,
   flavor, availability zone, and policy choices.
3. **Gateway infrastructure attributes:** portable infrastructure metadata
   where the Gateway API supports it.

A typed parameters resource is preferred to a growing annotation API. Its API
group and controller name must use a project-controlled domain chosen before
the first compatibility commitment.

## Failure and recovery model

- OpenStack asynchronous states are observed and requeued without tight loops.
- Retryable and terminal failures are distinguished.
- Kubernetes status shows the last useful error without exposing secrets.
- Partial resource creation is recoverable.
- External deletion causes safe recreation unless the Kubernetes object is
  deleting.
- Name collision with an unowned resource is terminal until an operator
  resolves it.
- Finalization has a bounded retry policy and an operator procedure for
  genuinely unrecoverable resources.

## Security model

- Prefer Keystone application credentials with least privilege.
- Credentials are loaded from Secrets and never emitted in logs or status.
- RBAC grants only required Kubernetes reads and writes.
- Network IDs and OpenStack resource IDs are not treated as credentials, but
  private environment details should not appear in public test fixtures.
- Image releases should include SBOM, provenance, signature, and vulnerability
  scanning before production readiness is claimed.

## Native and proxy-backed paths

The native and proxy-backed paths solve different problems.

### Native

```mermaid
flowchart LR
    C["Client"] --> F["Floating IP"]
    F --> O["Octavia L7"]
    O --> B["Backend"]
```

### Traefik reference profile

```mermaid
flowchart LR
    C["Client"] --> O["Octavia L4"]
    O --> T["Traefik"]
    T --> B["Backend"]
```

The Traefik path can provide broader HTTP behavior but retains an in-cluster
proxy and may use OCCM for Traefik's `LoadBalancer` Service. It is a reference
profile, not a mode of the native controller.

## Open design questions

- Which member mode should be the first supported default?
- Can a safe generated-NodePort strategy preserve Gateway API backend
  expectations?
- Which Octavia L7 behaviors pass the Gateway HTTP Core test suite?
- What provider identity can be detected reliably at runtime?
- Which project-controlled DNS domain should back the controller name and API
  group?
- Which fields belong in class parameters versus portable Gateway
  infrastructure attributes?

Each question must be closed by an ADR before the corresponding API is declared
stable.
