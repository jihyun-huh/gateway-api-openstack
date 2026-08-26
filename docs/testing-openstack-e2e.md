# Run the OpenStack controller E2E checks

This guide covers the first end-to-end checks for the current HTTP and
NodePort slice. The checks exercise a real Kubernetes API server and an
OpenStack Amphora data plane. They do not run the Gateway API conformance suite
and they do not establish a supported OpenStack environment by themselves.

No completed controller E2E report has been published yet. Running a local
command is not evidence until the environment, controller artifact, results,
and redacted supporting records are captured in an
[OpenStack E2E report](reports/openstack-e2e-template.md).

## Use an isolated project

The checks create and delete Octavia and Neutron resources. Use a disposable,
dedicated OpenStack project that contains no customer workloads and no
resources owned by another controller. Do not point the checks at a production
or shared project.

Before starting:

- Confirm the kubeconfig context and cluster name.
- Confirm the OpenStack cloud, region, and authenticated project.
- Confirm that the credentials cannot access projects outside the test scope.
- Confirm that the VIP, member, and optional external networks are intended for
  this test.
- Record a redacted starting inventory for the project.

Stop if any project, network, or controller identity is uncertain. Names and
cached IDs do not prove that an existing resource belongs to this controller.
The test must not adopt, modify, or delete an existing resource to make the
project look clean.

## Prerequisites

Prepare all of the following before running the checks:

- A Kubernetes cluster with the Gateway API v1.6.1 Standard Channel CRDs.
- A test kubeconfig authorized to create and delete the suite resources, read
  Gateway API CRDs, Gateways, HTTPRoutes, EndpointSlices, ReplicaSets, Pods,
  and the leader Lease. It must also update the dedicated controller
  Deployment, delete the current leader Pod, and get the current leader Pod
  through the `pods/proxy` subresource. The audit requires cluster-wide list
  access to Gateways and HTTPRoutes.
- An Octavia deployment where the literal load balancer provider is
  `amphora`.
- A network path from Amphora instances to the selected Node addresses and
  NodePorts.
- A dedicated OpenStack project with enough quota for one load balancer, its
  listener and route resources, and an optional Floating IP.
- A controller image built from the revision under test and pushed to a
  registry the cluster can pull from.
- A Kubernetes Secret containing the test project credentials, following the
  [getting started guide](getting-started.md).

Use the controller image by immutable digest, for example
`registry.example/controller@sha256:...`. A mutable tag alone is not enough for
a retained report. Record both the source release or commit and the digest.
This repository does not currently publish a supported controller image.

Do not put `clouds.yaml`, application credentials, tokens, kubeconfigs,
certificates, or unredacted resource inventories in the repository or command
logs.

## Configure the harness

The harness is opt-in. It does nothing unless `GATEWAY_OPENSTACK_E2E` is
exactly `true`, and it validates the rest of the settings before starting the
suite. Keep the values in the local environment used for the run. Do not commit
a populated environment file.

Set these required variables:

| Variable | Value |
| --- | --- |
| `GATEWAY_OPENSTACK_E2E_DEDICATED_PROJECT` | Exactly `true`, acknowledging that the OpenStack project is dedicated to this run |
| `GATEWAY_OPENSTACK_E2E_RUN_ID` | A unique DNS label between 8 and 32 characters |
| `GATEWAY_OPENSTACK_E2E_NAMESPACE` | Exactly `gateway-api-openstack-e2e-<run-id>` |
| `GATEWAY_OPENSTACK_E2E_KUBECONFIG` | An absolute path to the test kubeconfig |
| `GATEWAY_OPENSTACK_E2E_CONTEXT` | The exact context in that kubeconfig |
| `GATEWAY_OPENSTACK_E2E_CONTROLLER_NAME` | The controller name used by the dedicated installation |
| `GATEWAY_OPENSTACK_E2E_CONTROLLER_NAMESPACE` | The namespace of the dedicated controller Deployment |
| `GATEWAY_OPENSTACK_E2E_CONTROLLER_DEPLOYMENT` | The controller Deployment name |
| `GATEWAY_OPENSTACK_E2E_CONTROLLER_CONTAINER` | The controller container name |
| `GATEWAY_OPENSTACK_E2E_CONTROLLER_REPLICAS` | At least `2`, so leader recovery can be checked |
| `GATEWAY_OPENSTACK_E2E_CONTROLLER_IMAGE_DIGEST` | The lowercase `sha256:` digest of the deployed controller image |
| `GATEWAY_OPENSTACK_E2E_CONTROLLER_REVISION` | The full 40-character lowercase Git commit used to build the controller image |
| `GATEWAY_OPENSTACK_E2E_LEASE_NAME` | The leader election Lease name |
| `GATEWAY_OPENSTACK_E2E_BACKEND_IMAGE` | A complete `agnhost` image reference pinned with `@sha256:` |
| `GATEWAY_OPENSTACK_E2E_ARTIFACT_DIR` | A non-root absolute path whose final component is exactly the run ID. The Make target sets it from `E2E_ARTIFACT_DIR` |

The controller namespace must differ from the workload namespace. The
dedicated Deployment must already carry
`e2e.gateway-api-openstack.io/run-id: <run-id>`, and its replicas, image digest,
leader election configuration, metrics configuration, and Lease must match the
values above. The controller container must have exactly one
`--leader-elect=true` argument, one `--metrics-bind-address=:8080` argument,
and one named TCP `metrics` container port at 8080. These checks keep the
harness from restarting an ambient controller Deployment.

The following example shows the shape of a run. Replace every value with the
dedicated environment you checked. The digest strings are shortened here and
will be rejected if they are not complete lowercase SHA-256 values.

```sh
export GATEWAY_OPENSTACK_E2E=true
export GATEWAY_OPENSTACK_E2E_DEDICATED_PROJECT=true
export GATEWAY_OPENSTACK_E2E_RUN_ID=run-20260820
export GATEWAY_OPENSTACK_E2E_NAMESPACE=gateway-api-openstack-e2e-run-20260820
export GATEWAY_OPENSTACK_E2E_KUBECONFIG=/absolute/path/to/e2e.kubeconfig
export GATEWAY_OPENSTACK_E2E_CONTEXT=openstack-e2e
export GATEWAY_OPENSTACK_E2E_CONTROLLER_NAME=example.test/gateway-api-openstack
export GATEWAY_OPENSTACK_E2E_CONTROLLER_NAMESPACE=openstack-gateway-system
export GATEWAY_OPENSTACK_E2E_CONTROLLER_DEPLOYMENT=openstack-gateway-controller
export GATEWAY_OPENSTACK_E2E_CONTROLLER_CONTAINER=controller
export GATEWAY_OPENSTACK_E2E_CONTROLLER_REPLICAS=2
export GATEWAY_OPENSTACK_E2E_CONTROLLER_IMAGE_DIGEST=sha256:replace-with-64-lowercase-hex-characters
export GATEWAY_OPENSTACK_E2E_CONTROLLER_REVISION=replace-with-full-40-character-lowercase-git-commit
export GATEWAY_OPENSTACK_E2E_LEASE_NAME=gateway-api-openstack-controller
export GATEWAY_OPENSTACK_E2E_BACKEND_IMAGE=registry.k8s.io/e2e-test-images/agnhost:replace-with-version@sha256:replace-with-64-lowercase-hex-characters
export GATEWAY_OPENSTACK_E2E_AUDIT=true
export GATEWAY_OPENSTACK_E2E_CLUSTER_ID=replace-with-the-controller-cluster-id
export GATEWAY_OPENSTACK_E2E_AUDIT_CLOUDS_YAML=/absolute/path/to/clouds.yaml
export GATEWAY_OPENSTACK_E2E_AUDIT_CLOUD=openstack-e2e

make E2E_ARTIFACT_DIR=/absolute/path/to/e2e-artifacts/run-20260820 test-e2e
```

Without an `E2E_ARTIFACT_DIR` override, the Make target uses the absolute path
`_artifacts/e2e/<run-id>` below the repository. Run `make test-e2e-compile` to
compile the tagged suite without contacting a Kubernetes or OpenStack API.

`GATEWAY_OPENSTACK_E2E_RESTART_MODE` may be omitted or set to `cold`; no other
restart mode is accepted. The optional timing variables are
`GATEWAY_OPENSTACK_E2E_TIMEOUT`,
`GATEWAY_OPENSTACK_E2E_POLL_INTERVAL`,
`GATEWAY_OPENSTACK_E2E_HTTP_TIMEOUT`, and
`GATEWAY_OPENSTACK_E2E_NOOP_WINDOW`. Their defaults are `45m`, `5s`, `10s`, and
`30s` respectively. The suite timeout may not exceed `50m`. The default `45m`
covers the main scenario run. The Make target gives the Go test `90m` so that a
timed-out run still has up to `15m` for emergency cleanup, another `15m` for
the post-cleanup audit, and time to write the redacted artifact.

The suite starts the backend with `agnhost netexec --http-port=8080`. Use the
digest of the exact `agnhost` artifact available to the test cluster.

The harness reads metrics from the current leader without a separate metrics
URL. For each snapshot, it reads the configured Lease, matches its holder to a
Ready Pod owned by the dedicated Deployment, and opens that Pod's named
`metrics` port at TCP 8080 through the Kubernetes API `pods/proxy`
subresource. The Deployment must expose exactly that named container port, and
the kubeconfig identity must be allowed to get `pods/proxy`. A metrics Service
or a local port-forward is not required.

For the converged no-op assertion,
`gateway_api_openstack_openstack_mutations_total` must not increase during the
observation window, and the aggregate
`gateway_api_openstack_openstack_mutations_in_flight` value must be zero in
both snapshots. A nonzero in-flight value means the window did not bound a
complete set of mutation attempts and is not valid no-op evidence. The
`service="octavia",method="GET"` request counter and matching duration count
must increase, showing that the level-based controller made a fresh, read-only
Octavia observation after convergence.

The mutation counter is incremented when a potentially modifying request
starts, before the HTTP round trip. It uses only bounded `service` and `method`
labels. Requests other than GET, HEAD, and OPTIONS count conservatively unless
their endpoint is positively identified as Keystone.

The harness aggregates every `service` label value in the mutation counter.
Modifying requests classified as `unknown` still count, so an endpoint catalog
ambiguity cannot make the no-op check report a false zero.

The no-op check does not rely on the normal ten-minute resync happening during
the short observation window. After the first metrics snapshot, the harness
changes the HTTPRoute backendRef namespace from omitted to the explicit test
namespace. Those forms have the same Gateway API meaning and compile to the
same OpenStack graph. The harness waits for status to observe the new
generation before taking the second snapshot. The request and duration counts
must increase while the mutation attempt count stays unchanged.

The OpenStack counters begin after service discovery and reset when the
controller process restarts. Take both no-op snapshots from the current leader
process after restart recovery has converged. The harness checks
the exact `leader_election_master_status` Lease label,
`process_start_time_seconds`, the leader Pod UID, the controller container
start time and restart count, and the Lease holder and transition count around
the window. It rejects a leader or process change between snapshots. Do not
compare a counter from before a cold restart with one from the new process.

The read-only ownership audit is also opt-in. Set
`GATEWAY_OPENSTACK_E2E_AUDIT=true`,
`GATEWAY_OPENSTACK_E2E_CLUSTER_ID`, and the audit authentication settings for
the same project to collect aggregate counts before, during, and after the
scenario. The Make target builds the audit command and sets
`GATEWAY_OPENSTACK_E2E_AUDIT_BINARY` to its absolute path. When a clouds.yaml
file is used,
`GATEWAY_OPENSTACK_E2E_AUDIT_CLOUDS_YAML` must be an absolute path and
`GATEWAY_OPENSTACK_E2E_AUDIT_CLOUD` must name its entry. Region and Octavia
microversion overrides use `GATEWAY_OPENSTACK_E2E_AUDIT_REGION` and
`GATEWAY_OPENSTACK_E2E_AUDIT_OCTAVIA_MICROVERSION`; the microversion defaults
to `2.5`. Leaving the audit disabled keeps the required baseline-return check
`Not run` and prevents an overall `Passed` result.

The preflight audit must complete with no findings and an empty summary. Once
the Gateway graph is active, the audit must observe the two durable Kubernetes
bindings and only matched OpenStack resources. The harness compares an
in-memory fingerprint of that owned inventory after leader recovery and after
the cold restart. It does not write resource or object identifiers to the
artifact. After cleanup, the same audit must return to the empty baseline. A
known but unrelated binding or OpenStack resource still makes the project
unsuitable for this foundations run.

This comparison covers only resources visible to the ownership audit at each
snapshot. It cannot find a resource after all controller scope metadata is
removed, attribute an unmatched detached Floating IP, prove that the desired
graph is complete, or rule out a transient duplicate between snapshots. See
the [ownership audit contract](design/ownership-audit.md) for the full limits.
Use a separate, redacted project-wide inventory before and after the run to
make a leak claim.

## Run the baseline scenarios

Start with a clean project and keep the same controller image digest for the
whole run. Capture the following scenarios separately:

1. Verify that GatewayClass, Gateway, listener, and HTTPRoute conditions reach
   the expected state.
2. Send HTTP traffic through the address published in Gateway status and
   record the response.
3. Delete the current leader Pod and verify that another replica acquires the
   Lease, traffic recovers, and the active ownership inventory is unchanged.
4. Perform the harness's cold controller restart after convergence and verify
   that traffic remains available while the controller is stopped and that the
   same active ownership inventory is restored.
5. After restart recovery converges, apply the harness's equivalent backendRef
   namespace form and verify that its fresh OpenStack observation makes no
   mutation.
6. Delete the HTTPRoute, Gateway, GatewayClass, and test Namespace in dependency
   order and wait for finalization to complete.
7. Run the ownership audit and a redacted OpenStack inventory. Verify that the
   resources created for the test are absent.

The foundations harness uses `externalTrafficPolicy: Cluster` but does not
exercise member changes. Test changes in `Cluster` mode and
`externalTrafficPolicy: Local` as separate follow-up cases. Record the Node and
EndpointSlice change made in each case and the resulting Octavia member set.

## Run fault scenarios deliberately

Quota, timeout, Octavia failure, lost response, and blocked finalization tests
need a repeatable and safe fault mechanism. Run only the cases for which the
environment owner has approved that mechanism. Do not exhaust a shared quota,
modify another tenant's resources, or tamper with identity metadata to produce
a result.

The foundations harness does not inject a partial OpenStack transition. Keep
that report row as `Not run` unless a separate, reviewed mechanism creates a
repeatable checkpoint without weakening the ownership checks.

Leave a fault row as `Not run` when the mechanism is unavailable or unsafe.
Unit or adapter coverage does not turn an OpenStack E2E row into a pass. If a
cloud service or the test environment fails before the assertion can be made,
record an environment failure in the notes and keep the scenario from being
reported as passed.

For blocked finalization, retain the finalizer and follow the
[operator recovery guide](operator-recovery.md). Never remove a controller
finalizer by hand as part of the test.

## Record the result

Copy the [report template](reports/openstack-e2e-template.md) into a directory
named for the controller release or short commit. Fill in the exact environment
and artifact fields before changing any result from `Not run`.

Use these result meanings:

- `Passed` means the named assertion was observed and the report points to
  retained, redacted evidence.
- `Failed` means the test ran and the expected controller behavior was not
  observed.
- `Skipped` means an optional check was outside the configured run. It does not
  provide evidence for that check.
- `Not run` means the scenario was not attempted or did not reach the point
  where the assertion could be evaluated.

The harness writes `report.json` and `report.md` into a new artifact directory
and refuses to overwrite an existing run. Its overall `Passed` value requires
every foundations check, including the metrics no-op check and the post-test
ownership audit baseline return, to pass. Metrics and audit configuration
remain opt-in safeguards, but leaving either one out keeps the overall artifact
`Not run` and incomplete as Phase 2 evidence. Unexecuted fault scenarios do not
block the foundations result and remain `Not run`. Read the individual rows;
the overall value does not replace them.

The generated files are local supporting output. They record the source
revision, controller image digest, Gateway API bundle, restart mode, and check
summaries, but they do not replace the report template's environment,
topology, independent project-wide inventory, and publication review. A
`Passed` generated artifact therefore covers only the automated foundations
checks; it is not a complete Phase 2 evidence report by itself. Copy only
redacted evidence into the durable report.

A retry may be useful while diagnosing a problem, but it does not erase the
first unexplained result. Record both attempts and link a follow-up issue when
the cause remains unknown.

## Clean up safely

Delete the test HTTPRoute and Gateway before removing the controller,
credentials, or Gateway API CRDs. Wait for finalization. If cleanup stops,
leave the controller and credentials available and use the read-only ownership
audit to diagnose it.

After Kubernetes cleanup completes, compare an independent, project-wide final
inventory with the starting inventory. Report every remaining resource that
can be tied to the run. The scoped ownership audit baseline check does not
replace this comparison. Do not delete an unexpected resource unless its
complete immutable identity and surrounding graph have been validated.

Redact the final report once more before publishing it. In particular, check
command output, Events, object YAML, OpenStack request logs, and links to CI
artifacts for credentials, tokens, project and resource IDs, private
addresses, certificates, Pod names and UIDs, leader Lease holder identities,
and customer details.
