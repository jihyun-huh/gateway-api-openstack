# Roadmap

This roadmap sets the order of work for gateway-api-openstack. It is a planning
document, not a release promise. A feature moves forward only after its
ownership, failure behavior, and test evidence are clear.

The current development stage is **Phase 2**. The constrained Phase 1
implementation is complete in the source tree, but the project remains
pre-alpha. Completing an implementation phase does not by itself establish
production readiness, support, or conformance.

Each controller release pins the Gateway API version it supports. The current
baseline is the Gateway API v1.6.1 Standard Channel.

## Product contract

gateway-api-openstack is a Kubernetes Gateway API controller for HTTP and
terminated HTTPS traffic. It uses Octavia directly and supports only the
Amphora provider. It reconciles Gateway API resources into Octavia and Neutron
resources whose ownership can be verified before any mutation or deletion.
Terminated HTTPS also uses Barbican.

The goal is dependable behavior: predictable reconciliation, few unnecessary
OpenStack API calls, useful status, and safe upgrades. Application teams should
not need to understand OpenStack internals. The project does not aim to match
every feature or provider available in other cloud controllers.

The following boundaries are intentional:

- The controller accepts only the Amphora provider. Provider selection is not
  exposed to users, and every created load balancer must be verified as an
  Amphora load balancer. Release support remains limited to environments
  covered by published compatibility evidence.
- One Gateway owns one Octavia load balancer and its dependent graph.
- HTTP and terminated HTTPS are the data plane protocols in scope.
- By default, Amphora reaches backends through NodePorts when it cannot route
  directly to Pod IPs. Direct Pod IP members may be enabled later only for
  topologies whose routing has been verified.
- One controller deployment uses one OpenStack cloud, region, project, and
  credential scope. A project may run another deployment for another scope,
  with a distinct controller and cluster identity.
- The controller may resolve and validate existing networks, subnets, routers,
  ports, and security groups. It does not own the cloud network topology merely
  because it reads those resources.
- When a resource type is supported, the controller creates and manages only
  resources that belong to its Gateways: load balancers, listeners, pools,
  members, health monitors, L7 resources, Floating IPs, Barbican resources, and
  security resources covered by an accepted design.
- Existing load balancers, Floating IPs, certificates, and security groups are
  never adopted by the controller.
- Services of type `LoadBalancer` and every graph owned by the OpenStack Cloud
  Controller Manager (OCCM) remain the responsibility of
  cloud-provider-openstack.
- Ingress, proxy data planes, and other Octavia providers are not alternate
  modes of this controller.
- Gateway API behavior that Octavia cannot represent exactly is rejected in
  standard status. It is never silently ignored or approximated.

## Phase overview

The architecture document records the detailed ownership, identity, and
compiler rules. Accepted architecture decision records (ADRs) supersede its
proposed text. The table below summarizes each phase and its exit criteria.

| Phase | Status | Outcome | Exit gate |
| --- | --- | --- | --- |
| 0 — Feasibility | Historical | Prove the required Amphora primitives and ownership boundary | Capability probe retained, with later phases still requiring their ADRs |
| 1 — HTTP slice | Implemented (evidence pending) | One Gateway, listener, route, and NodePort backend | Published traffic, restart, no-op, deletion, and leak evidence |
| 2 — Reconciliation | In progress | One efficient writer for each Gateway graph that recovers after a restart | Fault, race, upgrade, API efficiency, and documented conformance gaps |
| 3 — Class API | Planned | Typed, validated topology configuration | Reviewed API contract, migration rules, and generated CRD |
| 4 — Connectivity | Planned | Deterministic address, network, and security handling | Results from tests in OpenStack without adopting foreign network resources |
| 5 — HTTP/HTTPS | Planned | Multiple listeners, route compilation, and terminated HTTPS | Pinned conformance results and safe Barbican lifecycle |
| 6 — Production candidate | Later | Repeatable operations and community evidence | Two independent clouds, hardened releases, and an eligible conformance report |
| 7 — Stable | Later | Supported, upgradeable Amphora contract | Stable API, adopter upgrades, support policy, and release evidence |

## Evidence and compatibility rules

Compatibility applies to a particular release and environment. A report must
record the Kubernetes, Gateway API, OpenStack, and Octavia versions, including
the negotiated API microversions. It must also describe the Amphora topology,
CNI and
kube-proxy mode, network relationships, enabled services, roles, quotas, and
the tests that passed.

`GatewayClass.status.supportedFeatures` is a runtime support declaration
consumed by the conformance suite, not a copy of an Octavia feature list or a
published conformance report. The controller publishes a feature only when
the corresponding behavior and every required feature combination have
passed the pinned Gateway API conformance suite. Declaring `Gateway` or
`HTTPRoute` means supporting that resource's full Core behavior. It cannot be
split into smaller feature names. A capability probe or a successful API call
is not enough.

Every phase must preserve the following invariants:

- the same desired state converges without further OpenStack mutations
- deletion can resume after a controller restart and cannot remove a foreign
  resource
- unsupported input is visible to the user before cloud mutation
- missing evidence is documented as missing rather than inferred
- public APIs remain compatible according to their declared maturity

## Phase 0 - Feasibility and ownership discovery

**Status:** Historical baseline — capability probe completed, design records
remain incomplete

**Milestone:** `phase-0-initialize`

Phase 0 established that an Amphora environment can create and inspect the
Octavia and Neutron primitives needed by the controller. It also established
the separate ownership boundary with cloud-provider-openstack and the
immutable identity requirements for safe deletion.

The retained probe is useful for environment discovery, but it is not a
controller end-to-end test. The architecture decisions originally identified
in this phase have not all been accepted as ADRs. Any unresolved decision is a
gate for the phase that depends on it. An implementation detail does not settle
the decision.

## Phase 1 - Constrained HTTP implementation

**Status:** Implementation complete — release evidence pending

**Milestone:** `v0.1.0`

Phase 1 implements the first end-to-end controller path:

- separate GatewayClass, Gateway, and HTTPRoute reconcilers built with
  controller-runtime and upstream Gateway API types
- selection by the exact controller name, with finalizers and stored identity
  for safe cleanup
- one HTTP listener, one selected route in the same namespace as the Gateway,
  one rule, and one NodePort Service backend in the route namespace
- exact hostname, Exact path, and PathPrefix matching by complete path elements
- one Octavia load balancer per Gateway, with an optional Floating IP created by
  the controller
- a pool, NodePort members, a health monitor, and L7 policies and rules for the
  selected HTTPRoute, with HTTPRoute identity included in ownership checks until
  the graph writer ADR is accepted
- standard status for the supported and explicitly rejected inputs
- a Gophercloud adapter boundary, unit tests, Kustomize deployment, example,
  and deterministic binary release packaging

The Phase 1 code is complete, but it still needs validation in an OpenStack
environment. The first published report must demonstrate traffic, status,
restart without duplication, converged no-op behavior, ordered deletion, and
absence of leaked resources in a documented Amphora topology. Phase 2 cannot
graduate without that report.

## Phase 2 - Safe and efficient reconciliation

**Status:** In progress

**Milestone:** `v0.2.0`

Phase 2 hardens the behavior implemented in Phase 1 before adding a CRD or more
Gateway API features.

### One writer for one Gateway graph

Gateway and HTTPRoute events can affect the same Octavia load balancer. The
controller will use the Gateway UID to identify and serialize changes to that
graph. Route reconcilers may remain responsible for attachment validation and
status, but one writer will compile and update listener policy order from the
complete desired graph.

The control loop follows these rules:

- Keep state for each reconciliation in local variables or a scope created for
  that request. Do not store loggers or objects in shared reconciler fields.
- Collect the Kubernetes and OpenStack state needed for one reconciliation
  before building the desired graph.
- Build and validate a deterministic desired graph without Gophercloud types.
- Compare only the fields owned by this controller.
- Apply the smallest mutation in dependency order.
- Make at most one asynchronous state transition, observe it for a bounded
  period, and then return.
- Requeue work that is still progressing in Octavia instead of holding a
  worker in a long poll.

A coordinator keyed by Gateway UID may serialize graph writes within the active
leader process. It must not become a global mutex, and it is not a substitute
for a level-based desired graph that recovers after process or leader failure.

### Failure, drift, and deletion

Provider outcomes distinguish pending progress, authentication or
authorization failure, quota exhaustion, rate limiting, request timeout,
retryable service failure, terminal validation, resource failure, and
ownership conflict. The controller returns transient failures to the workqueue
for backoff. Failures that need operator action use a slower retry interval
with jitter, while ownership conflicts are checked less often. The `status`
field and Events use fixed messages rather than OpenStack response bodies. An
OpenStack HTTP response code alone is not enough. For example, a `404` may
mean safe recreation or completed deletion, depending on the observed role
and phase.

Tests and reconciliation must cover:

- a load balancer without its listener
- a listener or pool with only part of the expected route graph
- a successful create whose response is lost before a controller restart
- resources created by the controller and later deleted externally
- drift in fields owned by this controller without overwriting foreign fields
- `PENDING_*`, Octavia `ERROR`, conflict, quota, rate limiting, and timeout
  outcomes
- repeated deletion and finalization that can resume after a restart
- identity loss or collision, which must stop mutation rather than trigger
  adoption or destructive repair

After a Gateway or HTTPRoute converges, the controller observes its OpenStack
resources again on a configurable slow interval. Stable, bounded jitter derived
from the Kubernetes resource UID spreads these observations over time. The same
scheduling rule applies to Octavia resources that are still progressing.
Kubernetes watches remain responsible for changes to desired state; periodic
observation finds changes made directly in OpenStack.

The OpenStack adapter recreates missing listeners, pools, members, and health
monitors after it verifies the remaining graph. It also enables a managed load
balancer or listener when its administrative state is changed outside the
controller. If route resources are present, the bound HTTPRoute first verifies
its exact identity, the complete Route graph, and the Gateway's provider,
subnet, listener, and Floating IP configuration. It then enables the listener
and load balancer in separate reconciliations. Gateway reconciliation performs
the repair directly only when there are no route resources. Adapter tests cover
these repairs. Fault injection in an OpenStack environment remains part of the
Phase 2 exit evidence.

Failures during finalization keep the finalizer and stored binding. The
controller records a stable condition and emits an Event when the condition
changes. If an HTTPRoute no longer has a valid parent status entry, an
annotation owned by this controller stores a cleanup reason that contains no
sensitive data. This lets the controller emit the Event once without inventing
a ParentStatus. The annotation is removed when cleanup makes progress or the
binding is cleared. Unit tests cover these rules, including redaction of
provider error details. OpenStack fault tests and the operator recovery
procedure still need validation in a disposable Amphora environment. The
experimental [ownership audit](docs/design/ownership-audit.md) now compares
Kubernetes ownership records with OpenStack resources discovered in the
authenticated project without deleting anything. The command and report have
unit coverage. The documented
[operator recovery workflow](docs/operator-recovery.md) still needs validation
in that environment.

The controller envtest uses a real Kubernetes API server and etcd to cover
status subresources, stale resource versions, durable binding checkpoints,
progressing finalization with a newly constructed reconciler, and cache field
indexes. It does not exercise a manager or process restart, run an OpenStack
data plane, or replace the fault tests required for this phase.

### Kubernetes and OpenStack API efficiency

Manager setup will index GatewayClass ownership, parent Gateway references,
backend Services, cleanup bindings, and the Service associated with each
EndpointSlice. Watch handlers will use indexed reads instead of listing every
object. Node changes will enqueue only affected managed NodePort backends, and
the controller will coalesce event bursts into one update to the member set.

The controller reuses shared OpenStack clients. Keystone, Octavia, and Neutron
requests use one configurable rate limit for the controller process. Bounded
contexts limit provider operations, and adapter list calls consume every page.
The reconcile path does not perform capability discovery, so it has no
capability result to cache. Increase worker concurrency only after race and
scale tests show that it improves throughput without overwhelming OpenStack.

### Conformance feasibility and design gates

Run the pinned GATEWAY-HTTP suite as a gap analysis and classify every result
as controller defect, compiler work, Octavia limitation, planned extension, or
out of scope. The known Core gaps include ClusterIP Services and any other
Service backend accepted by Gateway API but rejected by the current NodePort
path, `RequestHeaderModifier`, `RequestRedirect`, backend weights and invalid
backend semantics, and every applicable ReferenceGrant combination. Weights
assigned to Services must not be inferred from Octavia member weights.

Do not advertise `HTTPRoute` in `supportedFeatures` until all HTTPRoute Core
behavior has passed. Gateway API does not provide a declaration for only this
controller's smaller HTTPRoute subset. `ReferenceGrant` is included in
`supportedFeatures` only after all combinations with the other reported
features pass. If NodePort remains the only stable backend path, report partial
GATEWAY-HTTP conformance.

The GatewayClass controller checks the bundle version annotation on every
installed Gateway API CRD. It reports `SupportedVersion=True` only when the
required CRDs are present and the complete bundle matches v1.6.1. A missing or
mixed annotation makes the class `Accepted=False` with reason
`UnsupportedVersion`. Reconciliation leaves an existing OpenStack graph in
place, blocks further provisioning and repair, and still allows deletion and
controller handoff cleanup. CRD watches resume normal reconciliation after the
installed bundle becomes supported again.

Both the CRD watch and mutation checks use metadata rather than downloading
every CRD schema. Mutation checks also confirm live controller ownership. A
slow retry on the GatewayClass lets it recover even when a brief CRD update does
not produce another watch event. Dependent objects use that fallback only when
they observe a live mismatch before the class status changes. HTTPRoute cleanup
rebuilds the current attachment, backend, endpoint, Node, and route selection
decision before it removes a graph that still has a managed parent.

Before Phase 3 implementation begins, accept ADRs for:

1. the graph writer keyed by Gateway UID and ownership of route fragments
2. the canonical controller name and an API domain controlled by the project
3. GatewayClass and any parameter snapshot for individual Gateways, including
   merge, propagation, and migration semantics
4. shared ownership, if any, of the `security_groups` field on worker Neutron
   ports
5. the compatibility and deprecation policy for project CRDs

Also publish and review the contribution guide, code of conduct, support policy,
and ADR process before Phase 3 begins.

### Phase 2 exit criteria

- No two reconciles issue overlapping mutations to the same Gateway graph.
- A converged reconcile performs zero OpenStack mutations and no semantic
  status patch.
- Octavia pending states do not occupy a worker for an unbounded period.
- Restart, leader change, partial creation, external deletion, quota, rate
  limiting, timeout, repeat deletion, and `v0.1.x` upgrade tests pass.
- The controller keeps state local to each reconciliation request, and
  `go test -race ./...` passes.
- Indexed watch mapping replaces cluster-wide `List` calls used to find
  dependencies.
- The Phase 1 traffic, restart, deletion, and leak report from an OpenStack
  environment is published with exact environment versions and topology.
- The blocked finalization and ownership audit workflow is exercised in that
  environment without removing a finalizer or deleting an unverified resource.
- A public report identifies the remaining conformance gaps, and the required
  ADRs are public and reviewed.

## Phase 3 - Class configuration and topology API

**Status:** Planned

**Milestone:** `v0.3.0`

Phase 3 replaces a growing set of infrastructure flags for the controller
deployment with a small, typed GatewayClass configuration API. The provisional
kind name is OpenStackGatewayClassConfig. Its final group and kind require an
accepted ADR for the API domain.

Gateway API v1.6.1 also provides `Gateway.spec.infrastructure`, including a
namespaced `parametersRef`. Prefer that standard extension point over
implementation-specific annotations if a concrete OpenStack setting is needed
for a Gateway. Phase 3 must decide which settings application teams may safely
override on a Gateway and how they merge with class defaults before introducing
another configuration kind. A CRD for individual Gateways is not added merely
because the extension point exists. Until an accepted contract is implemented,
a Gateway `parametersRef` is rejected explicitly rather than ignored.

The initial `v1alpha1` resource will be cluster-scoped and referenced through
`GatewayClass.spec.parametersRef`, whose namespace must therefore be unset. It
may describe:

- VIP subnet selection
- Floating IP mode and external network selection
- Amphora flavor and availability zone
- NodePort member subnet and Node address type
- default health monitor settings for the class

Phase 4 may extend the alpha schema with reviewed modes for managing frontend
and backend security groups and explicit backend source CIDRs. The class API
should not expose a field before the controller has defined and tested its
behavior.

It will not contain provider selection, credentials, cloud, region, project,
arbitrary Octavia properties, or existing resource IDs for adoption. Amphora
is fixed by the product contract, while authentication scope remains a
deployment concern.

Start with OpenAPI schema and CEL validation. Add a webhook only when a rule
involving multiple fields cannot be expressed safely without one. Kubernetes
validation rejects structural errors. The controller still performs cloud
lookups and capability checks before accepting the class.

The API must define how class changes affect existing Gateways. By default, a
Gateway keeps the validated infrastructure binding recorded when it first
becomes managed. Later class changes must not silently move its load balancer
to another network. Any migration mechanism must be explicit, observable, and
able to resume after a restart.

During migration, a GatewayClass without `parametersRef` may use the Phase 1
deployment defaults. Deprecating those flags requires a separately announced
compatibility window. Cleanup must remain possible even if the parameter
object has been deleted or made invalid.

### Deterministic resource resolution

Network and subnet references may support an explicit UUID, an exact name, or
a Neutron tag selector. The API should make those choices mutually exclusive.
A selector must resolve to exactly one resource visible in the authenticated
project. Zero or multiple matches reject the class before mutation. Users must
enable automatic discovery explicitly, and it succeeds only when the result is
unambiguous.

An existing Gateway uses its validated binding instead of resolving a mutable
name or selector again. The controller does not create or delete networks,
subnets, routers, or routes as part of this API.

### Amphora capability checks

Before `Accepted=True`, the class reconciler verifies the Octavia endpoint, the
availability of the Amphora provider, required API and microversions, resource
tags, L7 operations, selected networks, and the configured project's read and
mutation permissions. Barbican and custom VIP security groups are checked only
when the class requests a feature that needs them.

### Phase 3 exit criteria

- A reviewed `v1alpha1` API, generated CRD, examples, and API reference are
  published under a domain controlled by the project.
- Invalid, missing, ambiguous, or unauthorized configuration is rejected
  before any OpenStack resource is created.
- More than one GatewayClass can select distinct validated topologies within
  the same authenticated project without confusing or sharing their owned
  resources.
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

- `Allocate` creates a Floating IP in the validated external network and
  attaches it to the Gateway.
- `Disabled` publishes the Octavia VIP and creates no Floating IP.
- Detachment or deletion of an owned Floating IP is recovered safely.
- Quota exhaustion and an ambiguous external network produce actionable
  conditions.
- Existing Floating IPs are not adopted, and foreign addresses on the VIP port
  produce an ownership conflict.

### Frontend security groups

`OctaviaManaged` remains the default: the controller omits `vip_sg_ids` and
Octavia manages listener access. A `Referenced` mode may pass security groups
supplied by the user through `vip_sg_ids` only after the controller verifies API
support. The controller validates the groups and does not change their rules.

Custom VIP security groups were added in Octavia API microversion 2.29 with
Octavia 16.0 (OpenStack 2025.1). They are applied to the VIP and Amphora VRRP
ports. Octavia retains its internal VRRP and HAProxy peer rules but no longer
manages listener ingress rules. The option is incompatible with listener
`allowed_cidrs` and SR-IOV load balancers. Those consequences must be
validated and visible before a Gateway is programmed. The currently pinned
Gophercloud version does not expose `vip_sg_ids` in its typed create options,
so implementation also requires an appropriate dependency update or a narrow,
tested extension. The initial implementation will rely on frontend security
groups managed by Octavia and will not create a separate frontend group.

### Backend security groups

Start with `Referenced` and `Unmanaged` modes. `Referenced` validates the
configuration supplied by the operator without modifying the referenced group.
`Unmanaged` leaves backend security to the environment. API discovery can
validate structural prerequisites, but it cannot prove that Amphora members can
reach NodePort backends. Octavia health status, an active reachability check, or
end-to-end test results from OpenStack must demonstrate that connectivity.

A `Managed` mode may be introduced only after its ADR defines a narrow shared
ownership contract. Worker Neutron ports belong to CAPO, another controller,
or the operator. Changing their `security_groups` list is a mutation of a
foreign object even when this controller created the group being attached.

If accepted, managed mode follows these rules:

- Enable it only through an explicit class opt-in.
- Create only groups and rules tagged with the controller's complete ownership
  identity.
- Map each Node provider ID to its Nova server UUID and selected address. Then
  validate exactly one Neutron port in the authenticated project by
  `device_owner`, `device_id`, selected fixed IP and subnet, and
  `port_security_enabled`.
- Preserve every foreign security group association. Add or remove only the
  group ID added by this controller.
- Include the observed Neutron revision in each update where the API supports
  it. If the port changes concurrently, reject the mutation and observe the
  port again before retrying.
- Reconcile only NodePorts still referenced by managed routes.
- Use exact ports by default. Opening the whole NodePort range requires an
  explicit choice between quota use and network exposure.
- Take backend source CIDRs from explicit configuration or verified
  environment evidence. Do not assume the VIP subnet is the source.
- Define the identity and reference counting for a shared group, along with
  restart recovery, uninstall, and rollback behavior.

Both `externalTrafficPolicy: Cluster` and `externalTrafficPolicy: Local` need
separate tests because their member selection differs. Node addition, removal,
address change, readiness, and EndpointSlice changes must converge the member
set and security access together without causing a burst of OpenStack API calls.

### Diagnostics

Add a read-only diagnostic command or dedicated binary for authentication,
Amphora and microversion checks, network resolution, mapping from each Node to
a Neutron port, NodePort reachability prerequisites, permissions to read and
update security groups, Floating IP quota, and optional Barbican access.
Diagnostic output must be redacted and suitable for attaching to an issue.

### Phase 4 exit criteria

- Network selection never chooses arbitrarily and does not mutate OpenStack
  when resolution is ambiguous.
- The controller safely recovers from Floating IP detachment and from drift in
  security rules that it created.
- Frontend `OctaviaManaged` and `Referenced` modes and backend `Referenced`
  and `Unmanaged` modes have documented results from an OpenStack environment.
- If `Managed` backend security is enabled, Node lifecycle and concurrent port
  changes pass E2E without changing or removing foreign groups or rules.
- The ADR for modifying worker ports owned by another component decides whether
  managed backend security will be supported. If managed mode is accepted, the
  tests must show that NodePort backends are reachable without manually
  changing the rules on each port. Otherwise, the operator remains responsible
  for the referenced configuration.
- No network, subnet, router, CAPO resource, or OCCM resource is adopted or
  deleted.

## Phase 5 - Complete the Amphora HTTP/HTTPS path

**Status:** Planned

**Milestone:** `v0.5.0`

Phase 5 adds Gateway API features after graph reconciliation and connectivity
are stable.

### Listener and route compiler

Support multiple Gateway listeners, multiple attached HTTPRoutes, multiple
rules, and multiple matches. Enforce `allowedRoutes`, namespace selectors,
`parentRef.sectionName`, and accurate `attachedRoutes` status.

The compiler owns the entire listener L7 graph. Gateway API matches in a rule
are alternatives, while conditions inside a match are conjunctive. Octavia L7
policies are ordered alternatives whose rules are conjunctive. All attached
routes therefore contribute to one deterministic policy order. Precedence,
tie-breaking, and Octavia policy positions must be stable across restarts and
map iteration.

Gateway API PathPrefix matches complete path elements, while Octavia
`STARTS_WITH` is a raw string prefix. `/foo` continues to compile into an exact
`/foo` policy and a `/foo/` prefix policy so it does not match `/foobar`.

If several Gateway listeners share a port, the controller may group them onto
one physical Octavia listener only when protocol, TLS, hostname, certificate,
and status semantics have been proven equivalent. Otherwise the unsupported
listener combination is rejected with standard listener conditions rather
than partially programmed.

### Supported HTTP behavior

Start with exact and wildcard hostnames, Exact and PathPrefix paths, default
routes, and exact header matches where the Amphora API can represent them
precisely. Request redirects may be added only for combinations whose scheme,
host, port, path, query, and HTTP status semantics match Gateway API.

Method and query matching, request or response header modification, URL
rewrite, request mirroring, arbitrary `ExtensionRef`, and timeouts or retries
configured for individual routes remain unsupported unless a later Octavia API
offers an exact mapping.
Unsupported or incompatible fields receive standard Gateway API conditions.

Weighted backends require the Phase 2 feasibility decision. Weights assigned to
Services, different endpoint counts, weight zero, and Gateway API behavior when
a backend is invalid must match the specification. Similar Octavia member
weights are not enough to prove this behavior. If exact semantics cannot be
demonstrated, the feature remains rejected and the project publishes partial
rather than full conformance.

### Cross-namespace references

Add ReferenceGrant handling for Service and Secret references. Removal of a
grant must revoke the reference and safely converge the dependent graph. Include
`ReferenceGrant` in `supportedFeatures` only after every applicable combination
with the controller's other reported features passes conformance.
Attachment of a route in another namespace to a Gateway follows Gateway API's
`allowedRoutes` rules and is not treated as a generic object reference grant.

### Terminated HTTPS

Support terminated HTTPS from Kubernetes TLS Secrets through Barbican resources
created by the controller. Start with one default certificate per listener. Add
SNI certificate sets after creation, rotation, rollback, and cleanup have been
tested.

The TLS design must cover Secret validation, redaction of private keys and
certificate data, cross-namespace authorization, immutable identity for
Barbican resources,
certificate rotation without downtime, crash recovery between old and new
certificates, and deletion of only the secrets and containers created by the
controller. If Barbican cannot expose enough identity to prove ownership, the
controller does not manage that resource type until a safe identity mechanism
has been designed and tested.

### Addresses and backend policy

Support representable `Gateway.spec.addresses` requests as Octavia VIP
addresses: dynamic allocation or a requested IP inside the validated VIP
subnet. Reject an unsupported address type, an address outside the binding, or
an address already in use. The class Floating IP policy remains a separate
choice for the external address. The API contract must state whether `Allocate`
adds a new FIP to a requested VIP or rejects that combination before mutation.

Introduce a namespaced OpenStackBackendPolicy only when documented use cases
require health monitor settings for individual Services. If added, the first
version will be an experimental `v1alpha1` Direct Policy Attachment. It will
use a `targetRef` to a Service in the same namespace, or to a named Service port
through `sectionName`, and contain only health monitor settings. The CRD carries
the `gateway.networking.k8s.io/policy: Direct` label and reports standard
`Accepted`, `Conflicted`, and `TargetNotFound` reasons through
`PolicyAncestorStatus` entries for each Gateway and controller. Session
persistence, pool algorithms, and connection limits do not belong in the first
version without separate evidence.

Support for direct Pod IP members remains optional and is not required for
`v1.0`. Enabling it requires checks that Amphora can route to Pod IPs, correct
handling of EndpointSlice ready, serving, and terminating conditions, safe
draining, CNI compatibility evidence, explicit security controls, and
documentation of source IP behavior. NodePort remains the default mode with the
broadest compatibility.

### Phase 5 exit criteria

- Multiple HTTP and terminated HTTPS listeners and routes share one Gateway
  without ownership conflicts or unstable policy order.
- Route precedence and PathPrefix matching by complete path elements pass unit
  tests, conformance tests, and tests in OpenStack.
- Cross-namespace Service and Secret access is granted and revoked exactly as
  Gateway API specifies.
- Certificate creation, rotation, restart recovery, and deletion leave no
  orphaned Barbican resources.
- Every reported `supportedFeatures` entry has a matching pinned conformance
  result.
- Any Core behavior that Amphora cannot represent is recorded openly. The
  release does not claim full GATEWAY-HTTP conformance in that case.
- Experimental backend policy or Pod IP support, if shipped, is clearly
  separated from the stable NodePort contract and has its own evidence.

## Phase 6 - Production candidate and community evidence

**Status:** Later

**Milestone:** `v1.0.0-beta.1`

Phase 6 validates installation, operation, troubleshooting, upgrades, and
removal outside the original development environment.

### Repeatable test environments

- Run scheduled E2E in at least two independently operated Amphora clouds with
  documented version and network differences.
- Cover NodePort in every environment. Test Pod IP only where that mode is
  claimed.
- Include paths with and without a Floating IP and an HTTPS environment with
  Barbican enabled.
- Exercise controller restart, leader change, credential rotation, Octavia
  failure, version upgrade, rollback, finalization, and leak detection.
- Publish measured scale results and behavior under bursts of EndpointSlice
  updates, including OpenStack API call budgets.

### Operations and security

- Publish metrics with bounded label values, alert guidance, dashboards, and
  useful Events.
- Harden and validate the Phase 2 guidance for troubleshooting, recovery from
  blocked finalization, ownership audits, and checks before uninstalling.
- Document the minimum required Keystone roles for Octavia, Neutron, and
  Barbican, along with credential rotation and revocation.
- Complete a threat model, Kubernetes RBAC review, tests that secrets are
  redacted, and dependency and container vulnerability scanning.
- Test backup, restore, storage version migration, and rollback for every
  project CRD.

### Releases and installation

- Publish a versioned controller image for each supported architecture and an
  OCI Helm chart in addition to Kustomize manifests.
- Produce reproducible artifacts with checksums, SBOMs, provenance, and
  signatures under a documented identity and promotion policy.
- Test clean install, upgrade between supported minor versions, rollback, and
  safe uninstall from the exact release artifacts.
- Publish an Amphora compatibility matrix, limitations, support window, and
  upgrade notes for each release.

### Community evidence

- Maintain contributor, governance, reviewer, release, support, and security
  documentation with roles that reflect actual project responsibility.
- Keep design decisions and compatibility reports public. Maintain a reliable
  development setup and publish focused `good first issue` and `help wanted`
  tasks.
- Collect feedback and publishable upgrade evidence from at least two external
  adopters.
- Submit a pinned conformance report upstream only when the submission can
  record partial results without overstating runtime `supportedFeatures`.
  Otherwise retain the public gap report until Core support makes an accurate
  submission possible. After Gateway API v1.6, an implementation needs at
  least a partially conformant report to join the upstream implementation
  list.

An eventual Kubernetes SIG or OpenInfra home is a community decision, not an
engineering milestone this repository can promise. Consider that step only
after the project has neutral governance, maintainers from more than one
organization, adopter evidence, and sustained upstream participation.

### Phase 6 exit criteria

- Scheduled E2E and upgrade tests pass in two independent Amphora
  environments.
- Tests at the published scale limits and API budgets handle the measured
  endpoint churn without leaks or a burst of OpenStack API calls.
- Common failures are diagnosable without database access or private cloud
  knowledge.
- Release images and charts are reproducible, signed, scanned, and documented.
- A conformance report for the release is published, with every skip or
  failure explained.
- No unresolved critical lifecycle or security defect blocks the release
  candidate.

## Phase 7 - Stable release

**Status:** Later

**Milestone:** `v1.0.0`

Stable means the supported contract can be operated and upgraded without
depending on repository internals or maintainer intervention.

The stable release requires all of the following:

- The class configuration API is at least `v1beta1`, with tested conversion,
  storage, defaulting, compatibility, and deprecation rules.
- Any experimental backend policy remains clearly versioned. A stable
  controller release does not make an experimental policy stable.
- At least two external adopters have completed an upgrade between supported
  minor versions and provided publishable evidence.
- The supported Kubernetes, Gateway API, OpenStack, Octavia, and CNI version
  policies and the release support window are public.
- The upstream conformance report and entry in the implementation list match the
  features the controller actually advertises.
- Scale limits, known limitations, troubleshooting, security and vulnerability
  response, and deprecation processes are public.
- Install, upgrade, rollback, credential rotation, and uninstall are
  reproducible using released artifacts.

Full GATEWAY-HTTP Core conformance is desirable because it gives users the
clearest portable contract. If the Phase 2 feasibility work proves that
Amphora cannot express a Core semantic safely, the stable contract will say so,
publish the conformance results, including failures, and remain useful within
its documented HTTP/HTTPS subset without overstating portability.

## Deferred or out of scope

The following do not belong to the core roadmap unless a later proposal with
supporting evidence changes the product contract:

- OVN or any non-Amphora Octavia provider
- Services of type `LoadBalancer`, Ingress, or migration of OCCM resources
- adoption of existing load balancers, Floating IPs, certificates, security
  groups, or other cloud resources
- creation or ownership of tenant networks, subnets, routers, and routes
- Traefik, Envoy, or another in-cluster proxy data plane
- TCPRoute, UDPRoute, TLSRoute, GRPCRoute, ListenerSet, GAMMA, service mesh, and
  multi-cluster Gateway
- managing several OpenStack projects or credential scopes from one controller
  deployment
- arbitrary annotations or extension fields that bypass a reviewed API
- HTTP transformation or traffic behavior that the public Octavia API cannot
  represent with Gateway API semantics

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
