# Roadmap

This roadmap describes the order in which gateway-api-openstack intends to
become useful and trustworthy. It is a working plan, not a release commitment.
Features move forward only when their ownership, failure behavior, and test
evidence are understood.

The current development stage is **Phase 2**. The constrained Phase 1
implementation is complete in the source tree, but the project remains
pre-alpha. A completed implementation phase is not, by itself, a support,
production-readiness, or conformance claim.

Each controller release pins the Gateway API version it supports. The current
baseline is the Gateway API Standard Channel from v1.6.1.

## Product contract

gateway-api-openstack is an **Amphora-only, Octavia-native Kubernetes Gateway
API controller for HTTP and terminated HTTPS traffic**. It turns portable
Gateway API intent into an identity-safe graph of Octavia and Neutron
resources, with Barbican resources added for TLS.

The project aims for the operational quality of a mature cloud load balancer
controller: predictable reconciliation, low OpenStack API churn, useful
status, safe upgrades, and as little cloud-specific work for application teams
as the environment permits. Completeness means doing the supported job well;
it does not mean matching every feature or provider supported by another cloud
controller.

The following boundaries are intentional:

- The controller accepts only the Amphora provider. Provider selection is not
  a user-facing feature, and every created load balancer must be verified as
  an Amphora load balancer. Release support remains limited to environments
  covered by published compatibility evidence.
- One `Gateway` owns one Octavia load balancer and its dependent graph.
- HTTP and terminated HTTPS are the data-plane protocols in scope.
- NodePort is the default and compatibility backend path. Direct Pod IP
  members may be enabled later only for explicitly verified routable
  topologies.
- One controller deployment uses one OpenStack cloud, region, project, and
  credential scope. A project may run another deployment for another scope,
  with a distinct controller and cluster identity.
- The controller may resolve and validate existing networks, subnets, routers,
  ports, and security groups. It does not own the cloud network topology merely
  because it reads those resources.
- When a resource type is supported, the controller creates and manages only
  its own load balancers, listeners, pools, members, health monitors, L7
  resources, Floating IPs, Barbican resources, and explicitly designed
  security resources.
- Existing load balancers, Floating IPs, certificates, and security groups are
  never adopted as controller-owned resources.
- `Service type=LoadBalancer` and every OCCM-owned graph remain the
  responsibility of cloud-provider-openstack.
- Ingress, proxy data planes, and other Octavia providers are not alternate
  modes of this controller.
- Gateway API behavior that Octavia cannot represent exactly is rejected in
  standard status. It is never silently ignored or approximated.

## Phase overview

The architecture document records the detailed ownership, identity, and
compiler rules; accepted ADRs supersede its proposed text. This table is the
shortest view of the delivery order and its gates.

| Phase | Status | Outcome | Exit gate |
| --- | --- | --- | --- |
| 0 — Feasibility | Historical | Prove the required Amphora primitives and ownership boundary | Retained capability probe; dependent ADRs remain explicit gates |
| 1 — HTTP slice | Implemented; evidence pending | One Gateway, listener, route, and NodePort backend | Published traffic, restart, no-op, deletion, and leak evidence |
| 2 — Reconciliation | In progress | One efficient, restart-safe writer for each Gateway graph | Fault, race, upgrade, API-efficiency, and conformance-gap evidence |
| 3 — Class API | Planned | Typed, validated topology configuration | Reviewed API contract, migration rules, and generated CRD |
| 4 — Connectivity | Planned | Deterministic address, network, and security handling | Real-cloud evidence without adopting foreign network resources |
| 5 — HTTP/HTTPS | Planned | Multi-listener and route compilation plus terminated HTTPS | Pinned conformance results and safe Barbican lifecycle |
| 6 — Production candidate | Later | Repeatable operations and community evidence | Two independent clouds, hardened releases, and an eligible conformance report |
| 7 — Stable | Later | Supported, upgradeable Amphora contract | Stable API, adopter upgrades, support policy, and release evidence |

## Evidence and compatibility rules

Compatibility is release- and environment-specific. A report must identify
the Kubernetes, Gateway API, OpenStack, Octavia, and API microversions; Amphora
topology; CNI and kube-proxy mode; VIP, member, and external networks; enabled
services; roles; quotas; and the tests that passed.

`GatewayClass.status.supportedFeatures` is a runtime support declaration
consumed by the conformance suite, not a copy of an Octavia feature list or a
published conformance report. The controller publishes a feature only when
the corresponding behavior and every required feature combination have
passed the pinned Gateway API conformance suite. Declaring `Gateway` or
`HTTPRoute` means supporting that resource's full Core behavior; it cannot be
split into smaller feature names. A capability probe or a successful API call
is not enough.

Every phase must preserve the following invariants:

- the same desired state converges without further OpenStack mutations;
- deletion is restart-safe and cannot remove a foreign resource;
- unsupported input is visible to the user before cloud mutation;
- missing evidence is documented as missing rather than inferred; and
- public APIs remain compatible according to their declared maturity.

## Phase 0 - Feasibility and ownership discovery

**Status:** Historical baseline; capability probe completed, design records
remain incomplete

**Milestone:** `phase-0-initialize`

Phase 0 established that an Amphora environment can create and inspect the
Octavia and Neutron primitives needed by the controller. It also established
the non-overlapping ownership boundary with cloud-provider-openstack and the
immutable identity requirements for safe deletion.

The retained probe is useful for environment discovery, but it is not a
controller end-to-end test. The architecture decisions originally identified
in this phase have not all been accepted as ADRs. Any unresolved decision is a
gate for the phase that depends on it; it must not be treated as settled by an
implementation detail.

## Phase 1 - Constrained HTTP vertical slice

**Status:** Implementation complete; release evidence pending

**Milestone:** `v0.1.0`

Phase 1 provides the smallest complete controller path:

- separate `GatewayClass`, `Gateway`, and `HTTPRoute` reconcilers built with
  controller-runtime and upstream Gateway API types;
- exact controller-name selection and identity-safe finalizers;
- one HTTP listener, one selected same-namespace route, one rule, and one
  same-namespace NodePort Service backend;
- exact hostname, Exact path, and element-aware PathPrefix matching;
- one Octavia load balancer per Gateway, with an optional controller-owned
  Floating IP;
- route-scoped pools, NodePort members, health monitors, L7 policies, and L7
  rules within the Gateway-owned graph, carrying HTTPRoute identity under the
  current fail-closed implementation pending the graph-writer ADR;
- standard status for the supported and explicitly rejected inputs;
- a Gophercloud adapter boundary, unit tests, Kustomize deployment, example,
  and deterministic binary release packaging.

This implementation baseline does not close the evidence gap. The first
published real-cloud report still needs to demonstrate traffic, status,
restart without duplication, converged no-op behavior, ordered deletion, and
absence of leaked resources in a documented Amphora topology. Phase 2 cannot
graduate without that report.

## Phase 2 - Safe and efficient reconciliation

**Status:** In progress

**Milestone:** `v0.2.0`

Phase 2 makes the existing resource surface reliable before adding a CRD or a
broader Gateway API surface.

### One writer for one Gateway graph

Gateway and HTTPRoute events can affect the same Octavia load balancer. The
controller will use the Gateway UID as the graph and coordination key and will
define one writer for each complete desired graph. Route reconcilers may still
own attachment validation and status, but they must not race independent
partial views of listener policy order into the same load balancer.

The control loop will:

- keep request state local instead of storing loggers or objects on shared
  reconciler fields;
- observe one coherent Kubernetes and OpenStack snapshot;
- build and validate a deterministic provider-neutral desired graph;
- diff only fields owned by this controller;
- execute the smallest dependency-ordered mutation;
- perform at most one asynchronous state transition followed by bounded
  observation before returning; and
- requeue Octavia progress instead of holding a worker in a long poll.

A fine-grained keyed coordinator may serialize graph writes inside the active
leader. It must not become a global mutex, and it is not a substitute for a
level-based desired graph that recovers after process or leader failure.

### Failure, drift, and deletion

Provider outcomes will distinguish pending progress, authentication or
authorization failure, quota exhaustion, rate limiting, retryable service
failure, terminal validation, and ownership conflict. HTTP status alone is not
enough; for example, a `404` may mean safe recreation or completed deletion
depending on the observed role and phase.

Tests and reconciliation must cover:

- a load balancer without its listener;
- a listener or pool with only part of the expected route graph;
- a create response lost across controller restart;
- externally deleted controller-owned resources;
- owned-field drift without overwriting foreign fields;
- `PENDING_*`, Octavia `ERROR`, conflict, quota, rate-limit, and timeout
  outcomes;
- repeat deletion and restart-resumable finalization; and
- identity loss or collision, which must stop mutation rather than trigger
  adoption or destructive repair.

Finalizer failures will expose stable conditions, Events, and a documented
operator procedure. A read-only orphan audit will compare Kubernetes ownership
records with project-scoped OpenStack discovery without deleting anything.

### Kubernetes and OpenStack API efficiency

Manager setup will index GatewayClass ownership, normalized parent Gateways,
backend Services, cleanup bindings, and EndpointSlice-to-Service
relationships. Watch mappers will use indexed reads instead of full object
lists. Node changes will fan out only to managed NodePort backends that can be
affected, and bursts will be coalesced into one semantic member diff.

OpenStack clients and discovered Amphora-environment capabilities will be
shared, bounded, and invalidated when their credential or endpoint scope
changes. Client-side rate limits, pagination, timeouts, and measured
concurrency come before raising `MaxConcurrentReconciles`.

### Conformance feasibility and design gates

Run the pinned GATEWAY-HTTP suite as a gap analysis and classify every result
as controller defect, compiler work, Octavia limitation, planned extension, or
out of scope. The explicit Core gaps include ClusterIP and every other
non-`ExternalName` Service backend that the NodePort-only path rejects,
`RequestHeaderModifier`, `RequestRedirect`, backend weight and invalid-backend
semantics, and every applicable `ReferenceGrant` combination. Service-level
weights must not be inferred from Octavia member weights.

Do not advertise `HTTPRoute` in `supportedFeatures` until all HTTPRoute Core
behavior has passed; Gateway API does not provide a declaration for only this
controller's smaller HTTPRoute subset. `ReferenceGrant` is published only
after all combinations with the other reported features pass. If NodePort
remains the only stable backend path, partial GATEWAY-HTTP conformance is the
expected result to report. Report whether the installed Gateway API CRD
version is supported through the standard `SupportedVersion` condition.

Before Phase 3 implementation begins, accept ADRs for:

1. the Gateway-scoped graph writer and ownership of route fragments;
2. the canonical controller name and project-owned API domain;
3. GatewayClass and any per-Gateway parameter snapshot, merge, propagation,
   and migration semantics;
4. shared ownership, if any, of the security-group field on worker Neutron
   ports; and
5. the compatibility and deprecation policy for project CRDs.

In parallel, complete the remaining open-source project baseline by publishing
a contribution guide, code of conduct, support policy, and ADR process. These
are community interfaces and should be reviewed with the same care as code.

### Phase 2 exit criteria

- No two reconciles issue overlapping mutations to the same Gateway graph.
- A converged reconcile performs zero OpenStack mutations and no semantic
  status patch.
- Octavia pending states do not occupy a worker for an unbounded period.
- Restart, leader change, partial create, external deletion, quota, rate-limit,
  timeout, repeat deletion, and `v0.1.x` upgrade tests pass.
- Request-local reconciler state is race-tested with `go test -race ./...`.
- Indexed watch fan-out replaces full-list dependency scans.
- The Phase 1 real-cloud traffic, restart, deletion, and leak report is
  published with exact environment versions and topology.
- The conformance gap report and required ADRs are public and reviewed.

## Phase 3 - Class configuration and topology API

**Status:** Planned

**Milestone:** `v0.3.0`

Phase 3 replaces a growing set of deployment-wide infrastructure flags with a
small, typed GatewayClass configuration API. The provisional kind name is
`OpenStackGatewayClassConfig`; its final group and kind require the accepted
API-domain ADR.

Gateway API v1.6.1 also provides `Gateway.spec.infrastructure`, including a
namespaced `parametersRef`. Prefer that standard extension point over ad-hoc
annotations if a concrete per-Gateway OpenStack setting is needed. Phase 3 must
decide which settings an application-facing Gateway may safely override and
how they merge with class defaults before introducing another configuration
kind. A per-Gateway CRD is not added merely because the extension point exists.
Until an accepted contract is implemented, a per-Gateway parameters reference
is rejected explicitly rather than ignored.

The initial `v1alpha1` resource will be cluster-scoped and referenced through
`GatewayClass.spec.parametersRef`, whose namespace must therefore be unset. It
may describe:

- VIP subnet selection;
- Floating IP mode and external network selection;
- Amphora flavor and availability zone;
- NodePort member subnet and Node address type;
- class-level health-monitor defaults.

Phase 4 may extend the alpha schema with reviewed frontend and backend
security-group modes and explicit backend source CIDRs. The class API should
not expose a field before the controller has defined and tested its behavior.

It will not contain provider selection, credentials, cloud, region, project,
arbitrary Octavia properties, or existing resource IDs for adoption. Amphora
is fixed by the product contract, while authentication scope remains a
deployment concern.

Start with OpenAPI schema and CEL validation. Add a webhook only when a
cross-field rule cannot be expressed safely without one. Kubernetes validation
rejects structural errors; the controller still performs cloud lookups and
capability checks before accepting the class.

The API must define how class changes affect existing Gateways. The safe
default follows the GatewayClass template model: a Gateway keeps the validated
infrastructure binding captured when it first becomes managed, and a later
class change does not silently move an existing load balancer to another
network. Any migration mechanism must be explicit, observable, and
restart-safe.

For transition, a GatewayClass without `parametersRef` may use the Phase 1
deployment defaults. Deprecating those flags requires a separately announced
compatibility window. Cleanup must remain possible even if the parameter
object has been deleted or made invalid.

### Deterministic resource resolution

Network and subnet references may support an explicit UUID, an exact name, or
a Neutron tag selector. The API should make those choices mutually exclusive.
A selector must resolve to exactly one project-visible resource; zero or
multiple matches reject the class before mutation. Automatic discovery is an
explicit opt-in and succeeds only when the result is unambiguous.

The validated binding, not a mutable name or selector result, anchors an
existing Gateway. The controller does not create or delete networks, subnets,
routers, or routes as part of this API.

### Amphora capability preflight

Before `Accepted=True`, the class reconciler verifies the Octavia endpoint,
Amphora availability, required API and microversions, resource tags, L7
operations, selected networks, and the configured project's read and mutation
permissions. Barbican and custom VIP security groups are checked only when the
class requests a feature that needs them.

### Phase 3 exit criteria

- A reviewed `v1alpha1` API, generated CRD, examples, and API reference are
  published under a project-controlled domain.
- Invalid, missing, ambiguous, or unauthorized configuration is rejected
  before any OpenStack resource is created.
- More than one GatewayClass can select distinct validated topologies within
  the same authenticated project without crossing ownership.
- Existing Phase 1 Gateways survive upgrade and remain cleanable without the
  live parameter object.
- API defaulting, immutability, status, storage, upgrade, and deprecation rules
  have tests and documentation.

## Phase 4 - Network, address, and security automation

**Status:** Planned

**Milestone:** `v0.4.0`

Phase 4 reduces the manual Neutron work needed for the NodePort path without
taking ownership of the cluster's network topology.

### Floating IP lifecycle

Phase 1 already supports allocating a Floating IP from an explicit external
network ID. Phase 4 moves that choice into validated class policy and completes
the resolver and recovery behavior:

- `Allocate` creates and attaches a controller-owned Floating IP from the
  validated external network.
- `Disabled` publishes the Octavia VIP and creates no Floating IP.
- Detachment or deletion of an owned Floating IP is recovered safely.
- Quota exhaustion and an ambiguous external network produce actionable
  conditions.
- Existing Floating IPs are not adopted, and foreign addresses on the VIP port
  produce an ownership conflict.

### Frontend security groups

`OctaviaManaged` remains the default: the controller omits `vip_sg_ids` and
Octavia manages listener access. A capability-gated `Referenced` mode may pass
user-owned security groups through `vip_sg_ids`, but the controller validates
them and does not change their rules.

Custom VIP security groups were added in Octavia API microversion 2.29 with
Octavia 16.0 (OpenStack 2025.1). They are applied to the VIP and Amphora VRRP
ports. Octavia retains its internal VRRP and HAProxy peer rules but no longer
manages listener ingress rules. The option is incompatible with listener
`allowed_cidrs` and SR-IOV load balancers. Those consequences must be
validated and visible before a Gateway is programmed. The currently pinned
Gophercloud version does not expose `vip_sg_ids` in its typed create options,
so implementation also requires an appropriate dependency update or a narrow,
tested extension. A controller-created frontend group is not required while
Octavia's default management provides the safer path.

### Backend security groups

Start with `Referenced` and `Unmanaged` modes. `Referenced` validates the
operator-provided connectivity contract without modifying the referenced
group. `Unmanaged` leaves backend security entirely to the environment. The
controller can validate structural prerequisites, but API discovery alone
cannot prove Amphora-to-NodePort reachability; actual connectivity is shown by
health state, an active diagnostic, or real-cloud E2E evidence.

A `Managed` mode may be introduced only after its ADR defines a narrow shared
ownership contract. Worker Neutron ports belong to CAPO, another controller,
or the operator; changing their `security_groups` list is a mutation of a
foreign object even when the group being attached is controller-owned.

If accepted, managed mode must:

- be an explicit class opt-in;
- create only identity-tagged controller-owned groups and rules;
- map each Node provider ID to its Nova server UUID and selected address, then
  validate exactly one project-scoped Neutron port by `device_owner`,
  `device_id`, selected fixed IP and subnet, and `port_security_enabled`;
- preserve every foreign security-group association and add or remove only
  the controller-owned group ID;
- use revision-aware updates where the cloud supports them and fail closed on
  concurrent change;
- reconcile only NodePorts still referenced by managed routes;
- use exact ports by default, with a whole NodePort range as an explicit
  quota-versus-exposure tradeoff;
- obtain backend source CIDRs from explicit configuration or verified
  environment evidence, never by assuming the VIP subnet is the source; and
- define shared-group identity, reference counting, restart recovery,
  uninstall, and rollback behavior.

Both `externalTrafficPolicy: Cluster` and the current `Local` behavior need
separate tests because their member selection differs. Node addition, removal,
address change, readiness, and EndpointSlice changes must converge members and
security access together without an API storm.

### Diagnostics

Add a read-only diagnostic command or dedicated binary for authentication,
Amphora and microversion checks, network resolution, Node-to-port mapping,
NodePort reachability prerequisites, security-group permissions, Floating IP
quota, and optional Barbican access. Diagnostic output must be redacted and
suitable for attaching to an issue.

### Phase 4 exit criteria

- Network selection never chooses arbitrarily and makes no mutation on an
  ambiguous result.
- Floating IP detach and controller-owned security rule drift recover safely.
- Frontend `OctaviaManaged` and `Referenced` modes and backend `Referenced`
  and `Unmanaged` modes have documented real-cloud results.
- If `Managed` backend security is enabled, Node lifecycle and concurrent port
  changes pass E2E without changing or removing foreign groups or rules.
- The shared-port ADR records a clear go or no-go decision. If managed mode is
  accepted, its tested path reaches NodePort backends without manual per-port
  rule changes; otherwise the documented referenced path remains explicit
  operator configuration rather than implied automation.
- No network, subnet, router, CAPO resource, or OCCM resource is adopted or
  deleted.

## Phase 5 - Complete the Amphora HTTP/HTTPS path

**Status:** Planned

**Milestone:** `v0.5.0`

Phase 5 broadens the northbound API only after graph reconciliation and
connectivity are stable.

### Listener and route compiler

Support multiple Gateway listeners, multiple attached HTTPRoutes, multiple
rules, and multiple matches. Enforce `allowedRoutes`, namespace selectors,
`parentRef.sectionName`, and accurate `attachedRoutes` status.

The compiler owns the entire listener L7 graph. Gateway API matches in a rule
are alternatives, while conditions inside a match are conjunctive; Octavia L7
policies are ordered alternatives whose rules are conjunctive. All attached
routes therefore contribute to one deterministic policy order. Precedence,
tie-breaking, and Octavia policy positions must be stable across restarts and
map iteration.

Gateway API PathPrefix is element-aware, while Octavia `STARTS_WITH` is a raw
string prefix. `/foo` continues to compile into an exact `/foo` policy and a
`/foo/` prefix policy so it does not match `/foobar`.

If several Gateway listeners share a port, the controller may group them onto
one physical Octavia listener only when protocol, TLS, hostname, certificate,
and status semantics have been proven equivalent. Otherwise the unsupported
listener combination is rejected with standard listener conditions rather
than partially programmed.

### Supported HTTP behavior

The first expansion targets exact and wildcard hostnames, Exact and
PathPrefix paths, default routes, and exact header matches where the Amphora
API can represent them precisely. Request redirects may be added only for the
combinations whose scheme, host, port, path, query, and status-code semantics
match Gateway API.

Method and query matching, request or response header modification, URL
rewrite, request mirroring, arbitrary `ExtensionRef`, and per-route timeout or
retry remain unsupported unless a later Octavia API offers an exact mapping.
Unsupported or incompatible fields receive standard Gateway API conditions.

Weighted backends require the Phase 2 feasibility decision. Service-level
weights, different endpoint counts, weight zero, and invalid-backend traffic
must behave as Gateway API specifies; Octavia member weight similarity is not
proof. If exact semantics cannot be demonstrated, the feature remains
rejected and the project publishes partial rather than full conformance.

### Cross-namespace references

Add `ReferenceGrant` handling for Service and Secret references. Removal of a
grant must revoke the reference and safely converge the dependent graph.
The union feature is added to `supportedFeatures` only after every applicable
combination with the controller's other reported features passes conformance.
Cross-namespace route-to-Gateway attachment follows Gateway API's
`allowedRoutes` rules and is not treated as a generic object reference grant.

### Terminated HTTPS

Support terminated HTTPS from Kubernetes TLS Secrets through
controller-owned Barbican resources. Start with one default certificate per
listener; add SNI certificate sets after create, rotation, rollback, and
cleanup are proven.

The TLS design must cover Secret validation, private-data redaction,
cross-namespace authorization, Barbican immutable identity, certificate
rotation without downtime, crash recovery between old and new certificates,
and deletion of only controller-owned secrets and containers. If Barbican
cannot expose enough identity to prove ownership, the controller does not
manage that resource type until the gap is solved.

### Addresses and backend-specific policy

Support representable `Gateway.spec.addresses` requests as Octavia VIP
addresses: dynamic allocation or a requested IP inside the validated VIP
subnet. Reject an unsupported address type, an address outside the binding, or
an address already in use. The class Floating IP policy remains a separate
external-address choice; the API contract must state whether `Allocate` adds a
new FIP to a requested VIP or rejects that combination before mutation.

If real user demand establishes a need for per-Service health settings, a
namespaced `OpenStackBackendPolicy` may be introduced as an experimental
`v1alpha1` Direct Policy Attachment. Its first version should use a
same-namespace `targetRef` to a Service or named Service port through
`sectionName` and contain health-monitor settings only. The CRD carries the
`gateway.networking.k8s.io/policy: Direct` label and reports standard
`Accepted`, `Conflicted`, and `TargetNotFound` reasons with per-Gateway and
per-controller `PolicyAncestorStatus`. Session persistence, pool algorithms,
and connection limits do not belong in the first version without separate
evidence.

Direct Pod IP members remain an optional track, not a `v1.0` gate. Enabling
them requires a routability preflight, EndpointSlice ready/serving/terminating
semantics, safe draining, CNI-specific compatibility evidence, and explicit
security and source-IP documentation. NodePort remains the general
compatibility path.

### Phase 5 exit criteria

- Multiple HTTP and terminated HTTPS listeners and routes share one Gateway
  without ownership conflicts or unstable policy order.
- Route precedence and element-aware PathPrefix behavior pass unit,
  conformance, and real-cloud tests.
- Cross-namespace Service and Secret access is granted and revoked exactly as
  Gateway API specifies.
- Certificate create, rotation, restart recovery, and deletion leave no
  orphaned Barbican resources.
- Every reported `supportedFeatures` entry has a matching pinned conformance
  result.
- Any Core behavior that Amphora cannot represent is recorded openly; the
  release does not claim full GATEWAY-HTTP conformance in that case.
- Experimental backend policy or Pod IP support, if shipped, is clearly
  separated from the stable NodePort contract and has its own evidence.

## Phase 6 - Production candidate and community evidence

**Status:** Later

**Milestone:** `v1.0.0-beta.1`

Phase 6 proves that operators outside the original development environment
can install, operate, diagnose, upgrade, and remove the controller.

### Repeatable test environments

- Run scheduled E2E in at least two independently operated Amphora clouds with
  documented version and network differences.
- Cover NodePort in every environment; test Pod IP only where that mode is
  claimed.
- Include Floating IP enabled and disabled paths and a Barbican-enabled HTTPS
  environment.
- Exercise controller restart, leader change, credential rotation, Octavia
  failure, version upgrade, rollback, finalization, and leak detection.
- Publish scale and EndpointSlice-churn results with OpenStack API call budgets
  and measured limits rather than unbounded claims.

### Operations and security

- Publish bounded-cardinality metrics, alert guidance, dashboards, Events,
  troubleshooting flows, blocked-finalizer recovery, orphan audit, and
  uninstall preflight.
- Document least-privilege Keystone roles for Octavia, Neutron, and Barbican,
  along with credential rotation and revocation.
- Complete a threat model, Kubernetes RBAC review, secret-redaction tests, and
  dependency and container vulnerability scanning.
- Test backup, restore, storage-version migration, and rollback for every
  project CRD.

### Releases and installation

- Publish a versioned multi-architecture controller image and an OCI Helm
  chart in addition to Kustomize manifests.
- Produce reproducible artifacts with checksums, SBOMs, provenance, and
  signatures under a documented identity and promotion policy.
- Test clean install, minor-version upgrade, rollback, and safe uninstall from
  the exact release artifacts.
- Publish a release-specific Amphora compatibility matrix, limitations,
  support window, and upgrade notes.

### Community evidence

- Maintain contributor, governance, reviewer, release, support, and security
  documentation with roles that reflect actual project responsibility.
- Keep design decisions and compatibility reports public and provide focused
  `good first issue` and `help wanted` work with reliable development setup.
- Collect feedback and upgrade evidence from at least two external adopter
  environments.
- Submit a pinned conformance report upstream only when the submission can
  record partial results without overstating runtime `supportedFeatures`.
  Otherwise retain the public gap report until Core support makes an accurate
  submission possible. After Gateway API v1.6, an implementation needs at
  least a partially conformant report to join the upstream implementation
  list.

An eventual Kubernetes SIG or OpenInfra home is a community decision, not an
engineering milestone this repository can promise. The project first earns
that conversation through neutral governance, maintainers beyond one employer
or operator, adopter evidence, and sustained upstream participation.

### Phase 6 exit criteria

- Scheduled E2E and upgrade tests pass in two independent Amphora
  environments.
- Published scale limits and API budgets survive the tested endpoint churn
  without leaks or an OpenStack API storm.
- Common failures are diagnosable without database access or private cloud
  knowledge.
- Release images and charts are reproducible, signed, scanned, and documented.
- A version-specific conformance report is published, with every skip or
  failure explained.
- No unresolved critical lifecycle or security defect blocks the release
  candidate.

## Phase 7 - Stable release

**Status:** Later

**Milestone:** `v1.0.0`

Stable means the supported contract can be operated and upgraded without
depending on repository internals or maintainer intervention.

Before `v1.0.0`:

- the class configuration API is at least `v1beta1`, with tested conversion,
  storage, defaulting, compatibility, and deprecation rules;
- any experimental backend policy remains clearly versioned and is not
  presented as stable merely because the controller is stable;
- at least two external adopters have completed a supported minor-version
  upgrade and provided publishable evidence;
- supported Kubernetes, Gateway API, OpenStack, Octavia, and CNI version
  policies and the release support window are public;
- the upstream conformance report and implementation-list entry match the
  features the controller actually advertises;
- scale limits, known limitations, troubleshooting, security response, CVE,
  and deprecation processes are public; and
- install, upgrade, rollback, credential rotation, and uninstall are
  reproducible using released artifacts.

Full GATEWAY-HTTP Core conformance is desirable because it gives users the
clearest portable contract. If the Phase 2 feasibility work proves that
Amphora cannot express a Core semantic safely, the stable contract will say so,
publish a partial conformance report, and remain useful within its documented
HTTP/HTTPS subset without overstating portability.

## Deferred or out of scope

The following do not belong to the core roadmap unless a later, evidence-backed
proposal changes the product contract:

- OVN or any non-Amphora Octavia provider;
- `Service type=LoadBalancer`, Ingress, or migration of OCCM resources;
- adoption of existing load balancers, Floating IPs, certificates, security
  groups, or other cloud resources;
- creation or ownership of tenant networks, subnets, routers, and routes;
- Traefik, Envoy, or another in-cluster proxy data plane;
- TCPRoute, UDPRoute, TLSRoute, GRPCRoute, ListenerSet, GAMMA, service mesh, and
  multi-cluster Gateway;
- managing several OpenStack projects or credential scopes from one controller
  deployment;
- arbitrary annotations or extension fields that bypass a reviewed API; and
- HTTP transformation or traffic behavior that the public Octavia API cannot
  represent with Gateway API semantics.

## Technical references

- [Gateway API implementer's guide](https://gateway-api.sigs.k8s.io/guides/implementers-guide/)
- [GatewayClass parameters](https://gateway-api.sigs.k8s.io/reference/api-types/gatewayclass/)
- [Per-Gateway infrastructure (GEP-1867)](https://gateway-api.sigs.k8s.io/geps/gep-1867/)
- [Gateway API policy attachment](https://gateway-api.sigs.k8s.io/reference/policy-attachment/)
- [Gateway API implementation listing](https://gateway-api.sigs.k8s.io/docs/implementations/list/)
- [Gateway API v1.6.1 release](https://github.com/kubernetes-sigs/gateway-api/releases/tag/v1.6.1)
- [Gateway API conformance tests](https://github.com/kubernetes-sigs/gateway-api/tree/main/conformance)
- [Octavia API v2](https://docs.openstack.org/api-ref/load-balancer/v2/)
- [Octavia custom VIP security groups](https://docs.openstack.org/octavia/latest/contributor/specs/version15.0/custom-security-groups-for-VIP-ports.html)
- [AWS Load Balancer Controller design](https://kubernetes-sigs.github.io/aws-load-balancer-controller/latest/how-it-works/)
- [AWS Load Balancer Controller security groups](https://kubernetes-sigs.github.io/aws-load-balancer-controller/latest/deploy/security_groups/)
- [Gophercloud](https://github.com/gophercloud/gophercloud)
- [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime)
- [cloud-provider-openstack](https://github.com/kubernetes/cloud-provider-openstack)
