# Run the OpenStack controller E2E checks

This guide covers the first end-to-end checks for the current HTTP and NodePort slice.
The checks exercise a real Kubernetes API server and an OpenStack Amphora data plane.
They do not run the Gateway API conformance suite and they do not establish a supported OpenStack environment by themselves.

No completed controller E2E report has been published yet.
Running a local command is not evidence until the environment, controller artifact, results, and redacted supporting records are captured in an [OpenStack E2E report](reports/openstack-e2e-template.md).

## Quick start for a dedicated cluster and shared project

Use this path when the Kubernetes cluster is reserved for the test but other people use the OpenStack project.
The command creates a controller installation for one run, executes the live OpenStack checks, and removes the installation only after the workload cleanup and ownership audit succeed.
It does not turn the controller's OpenStack tags into a Keystone permission boundary.
The `kubernetes.dedicatedForE2E` and `openstack.acceptProjectWideCredentialRisk` fields are operator acknowledgements.
The runner cannot prove that the cluster is isolated or that the credential is restricted to resources from this run.

Prepare these items before starting:

- A Kubernetes cluster reserved for the run, with Gateway API v1.6.1 Standard Channel CRDs installed.
- A kubeconfig and context with the permissions listed under [Prerequisites](#prerequisites), including permission to create and delete the run-scoped controller and its cluster-scoped RBAC.
- A controller image and an `agnhost` backend image that the cluster can pull, both pinned by digest.
  Set `controller.sourceRevision` to the full lowercase commit that produced the controller image and set `controller.domain` to a DNS domain you control.
- A controller `clouds.yaml` with only one cloud entry.
  That entry must be self-contained, use an application credential, and keep TLS verification enabled.
  Omit `verify` or set it to `true`.
  The runner rejects `verify: false`, profile inheritance, password or token authentication, and references to additional CA, client certificate, or client key files.
  The runner copies only the exact `clouds.yaml` bytes into the immutable Secret.
- An audit `clouds.yaml` with the same cloud entry name and region, unless the controller file is also suitable for the audit.
  The audit credential may be separate and should have only the inventory permissions it needs, although the preflight cannot prove which Keystone roles or policy rules it has.
- The exact OpenStack project, VIP subnet, member subnet, and external network approved for the test.
  Use the literal `none` when the run must not allocate a Floating IP.
- Enough project quota for the test graph and, when selected, one Floating IP.
- A network path from Amphora to the Node `InternalIP` and NodePort range, from the runner to both credentials' Keystone `auth_url`, and from the runner to the resulting VIP or Floating IP.
- A redacted project-wide starting inventory that the project users have reviewed.
  The runner audits only the exact controller and cluster scope, so it does not replace this independent record or the matching review after the run.

Copy the synthetic example to a private path and replace its placeholder values:

```sh
mkdir -p _artifacts/e2e-config
cp test/e2e/shared-project.example.yaml \
  _artifacts/e2e-config/shared-project.yaml
```

The parser rejects unknown fields and requires every path inside the YAML file to be absolute.
Only the top-level `runID`, `openstack.auditCloudsYAML`, and `artifacts` entries are optional.
Every other field in the example is required.
Keep `formatVersion: v1alpha1`, `kubernetes.dedicatedForE2E: true`, `openstack.projectMode: shared`, and `openstack.acceptProjectWideCredentialRisk: true` exactly as shown.
Set `openstack.externalNetworkID` to an exact network ID or the literal `none`.
Remove `openstack.auditCloudsYAML` to reuse `openstack.controllerCloudsYAML` for the audit.
Remove `artifacts` to use `_artifacts/e2e/<run-id>` below the repository, or set `artifacts.root` to a non-root absolute directory.
The runner appends the run ID to that artifact root.
The final `<artifacts.root>/<run-id>` directory must not already exist because the runner refuses to overwrite evidence.
You may set an explicit `runID` as a DNS label between 8 and 32 characters, but omit it for normal runs so the runner generates a new value.

Run the live suite with one command:

```sh
make test-e2e-shared \
  E2E_CONFIG="$PWD/_artifacts/e2e-config/shared-project.yaml"
```

This target does not run the unit, race, envtest, or compilation-only test targets.
Compiling the E2E runner and live test binary is still part of starting the command.

When `runID` is absent, the runner generates a new value and derives the Namespace, GatewayClass, controller name, cluster ID, RBAC names, and artifact directory.
Before it creates any controller object, the runner authenticates an isolated exact copy of the controller file and the audit configuration separately, and it requires both token projects to match `openstack.expectedProjectID`.
It then runs the ownership audit and requires a complete, empty result for the exact generated controller name and cluster ID.
If one of these checks fails, the runner stops without installing the controller.
It creates the immutable ConfigMap and Secret before the Deployment so the first controller Pod is already bound to their API identities.
It uses create-only Kubernetes writes and stops instead of adopting an object with the same name.
The YAML file is the source of truth for the run.
Inherited `GATEWAY_OPENSTACK_*` and `OS_*` variables do not override its generated harness environment.

If the live suite succeeds, the runner removes the run-scoped controller and RBAC.
If installation fails before the runner attempts to create the Deployment, it removes a partial installation only when every object still has the API UID and run annotation it recorded.
Once the runner attempts to create the Deployment, it does not run automatic cleanup after a create, rollout, suite, or final audit failure.
The run-scoped controller objects and clouds Secret remain available for review and finalization.
If removal after a successful suite fails, the runner stops at the first failed safety check or API operation.
Inspect all remaining run-scoped objects instead of assuming that cleanup completed.
Do not remove that installation, force a finalizer, or delete an OpenStack resource by name until the recovery checks in this guide show that the owned graph is absent.

The rest of this document describes prerequisites, the underlying environment variables, and the manual workflow used for debugging.

## Detailed project modes

The checks create and delete Octavia and Neutron resources.
A disposable, dedicated OpenStack project remains the preferred setup and is the expected setup for release evidence.
It gives the test its clearest failure and cleanup boundary.

The harness also has a guarded `shared` mode for an OpenStack project used by other engineers.
Use it only when the Kubernetes resources and controller installation are reserved for the run and the project owner accepts the remaining risk.
The shared checks establish a narrow logical scope.
They do not limit what the application credential can access inside the project.

Before starting either mode:

- Confirm the kubeconfig context and cluster name.
- Confirm the OpenStack cloud, region, and authenticated project.
- Confirm that the credentials cannot access projects outside the test scope.
- Confirm that the VIP, member, and optional external networks are intended for this test.
- Record a redacted starting inventory for the project.

Stop if any project, network, or controller identity is uncertain.
Names and cached IDs do not prove that an existing resource belongs to this controller.
The test must not adopt, modify, or delete an existing resource to make the project look clean.

In a shared project, controller requests can still consume project quota, compete for Octavia and Neutron API capacity, allocate a Floating IP, and reach resources allowed by the credential.
OpenStack policy normally does not turn controller tags into an authorization boundary.
The credential may still have project-wide load balancer, Floating IP, or security group permissions.
Do not run quota, fault injection, security group, or destructive recovery cases in shared mode.

## Prerequisites

Prepare all of the following before running the checks:

- A Kubernetes cluster with the Gateway API v1.6.1 Standard Channel CRDs.
- A test kubeconfig authorized to create and delete the suite resources, read Gateway API CRDs, Gateways, HTTPRoutes, EndpointSlices, ReplicaSets, Pods, and the leader Lease.
  It needs `get` access to the controller Namespace, ConfigMap, and Secret, and `list` access to GatewayClasses.
  Limit the namespaced reads to the run objects where RBAC permits.
  It must also update the run-scoped controller Deployment, delete the current leader Pod, and get the current leader Pod through the `pods/proxy` subresource.
  The audit requires cluster-wide list access to Gateways and HTTPRoutes.
  This runner also needs permission to create, get, and delete its Namespace, ServiceAccount, ClusterRole, ClusterRoleBinding, ConfigMap, Secret, and Deployment.
- An Octavia deployment where the literal load balancer provider is `amphora`.
- A network path from Amphora instances to the selected Node addresses and NodePorts.
- An OpenStack project with enough quota for one load balancer, its listener and route resources, and an optional Floating IP.
  Prefer a project dedicated to the test.
- A controller image built from the revision under test and pushed to a registry the cluster can pull from.
- Controller credentials suitable for a Kubernetes Secret, following the [getting started guide](getting-started.md).
  For quick start, the runner creates the run-scoped Secret from `openstack.controllerCloudsYAML`.
  The manual workflow uses a Secret prepared by the operator.
- In shared mode, a network path from the process running the E2E command to the Keystone `auth_url` in the controller Secret.
  The preflight authenticates that credential from the runner, outside the controller Pod.

Use the controller image by immutable digest, for example `registry.example/controller@sha256:...`.
A mutable tag alone is not enough for a retained report.
Record both the source release or commit and the digest.
This repository does not currently publish a supported controller image.

Do not put `clouds.yaml`, application credentials, tokens, kubeconfigs, certificates, or unredacted resource inventories in the repository or command logs.

## Configure the harness

The harness is opt-in.
It does nothing unless `GATEWAY_OPENSTACK_E2E` is exactly `true`, and it validates the rest of the settings before starting the suite.
Keep the values in the local environment used for the run.
Do not commit a populated environment file.

Set these required variables:

| Variable | Value |
| --- | --- |
| `GATEWAY_OPENSTACK_E2E_PROJECT_MODE` | Exactly `dedicated` or `shared` |
| `GATEWAY_OPENSTACK_E2E_RUN_ID` | A unique DNS label between 8 and 32 characters |
| `GATEWAY_OPENSTACK_E2E_NAMESPACE` | Exactly `gateway-api-openstack-e2e-<run-id>` |
| `GATEWAY_OPENSTACK_E2E_KUBECONFIG` | An absolute path to the test kubeconfig |
| `GATEWAY_OPENSTACK_E2E_CONTEXT` | The exact context in that kubeconfig |
| `GATEWAY_OPENSTACK_E2E_CONTROLLER_NAME` | The controller name used by the run-scoped installation |
| `GATEWAY_OPENSTACK_E2E_CONTROLLER_NAMESPACE` | The namespace of the run-scoped controller Deployment |
| `GATEWAY_OPENSTACK_E2E_CONTROLLER_DEPLOYMENT` | The controller Deployment name |
| `GATEWAY_OPENSTACK_E2E_CONTROLLER_CONTAINER` | The controller container name |
| `GATEWAY_OPENSTACK_E2E_CONTROLLER_REPLICAS` | At least `2`, so leader recovery can be checked |
| `GATEWAY_OPENSTACK_E2E_CONTROLLER_IMAGE_DIGEST` | The lowercase `sha256:` digest of the deployed controller image |
| `GATEWAY_OPENSTACK_E2E_CONTROLLER_REVISION` | The full 40-character lowercase Git commit used to build the controller image |
| `GATEWAY_OPENSTACK_E2E_LEASE_NAME` | The leader election Lease name |
| `GATEWAY_OPENSTACK_E2E_BACKEND_IMAGE` | A complete `agnhost` image reference pinned with `@sha256:` |
| `GATEWAY_OPENSTACK_E2E_ARTIFACT_DIR` | A non-root absolute path whose final component is exactly the run ID. The Make target sets it from `E2E_ARTIFACT_DIR` |

In `dedicated` mode, also set `GATEWAY_OPENSTACK_E2E_DEDICATED_PROJECT=true`.
This is an acknowledgement, not a feature switch.
For compatibility with the first version of the harness, omitting `GATEWAY_OPENSTACK_E2E_PROJECT_MODE` while setting the acknowledgement to exactly `true` still selects `dedicated` mode.
Set the mode explicitly for new runs.

The controller namespace must differ from the workload namespace.
The run-scoped Deployment must already carry `e2e.gateway-api-openstack.io/run-id: <run-id>`, and its replicas, image digest, leader election configuration, metrics configuration, and Lease must match the values above.
The controller container must have exactly one `--leader-elect=true` argument, one `--metrics-bind-address=:8080` argument, and one named TCP `metrics` container port at 8080.
These checks keep the harness from restarting an ambient controller Deployment.

The following example shows the shape of a run.
Replace every value with the dedicated environment you checked.
The digest strings are shortened here and will be rejected if they are not complete lowercase SHA-256 values.

```sh
export GATEWAY_OPENSTACK_E2E=true
export GATEWAY_OPENSTACK_E2E_PROJECT_MODE=dedicated
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

### Configure a shared project run

Use a new identity for every shared run.
The harness derives several values from the run ID and rejects an approximate match.
In this mode, `GATEWAY_OPENSTACK_E2E_DEDICATED_PROJECT` must be unset or exactly `false`.

```sh
RUN_ID=leaf-e2e-20260828a

export GATEWAY_OPENSTACK_E2E=true
export GATEWAY_OPENSTACK_E2E_PROJECT_MODE=shared
export GATEWAY_OPENSTACK_E2E_DEDICATED_PROJECT=false
export GATEWAY_OPENSTACK_E2E_RUN_ID="$RUN_ID"
export GATEWAY_OPENSTACK_E2E_NAMESPACE="gateway-api-openstack-e2e-$RUN_ID"

export GATEWAY_OPENSTACK_E2E_CONTROLLER_NAME="example.test/gao-e2e-$RUN_ID"
export GATEWAY_OPENSTACK_E2E_CONTROLLER_NAMESPACE="openstack-gateway-e2e-$RUN_ID"
export GATEWAY_OPENSTACK_E2E_CONTROLLER_DEPLOYMENT=openstack-gateway-controller
export GATEWAY_OPENSTACK_E2E_CONTROLLER_CONTAINER=controller
export GATEWAY_OPENSTACK_E2E_CONTROLLER_REPLICAS=2
export GATEWAY_OPENSTACK_E2E_LEASE_NAME=gateway-api-openstack-controller

export GATEWAY_OPENSTACK_E2E_EXPECTED_PROJECT_ID=replace-with-exact-project-id
export GATEWAY_OPENSTACK_E2E_EXPECTED_VIP_SUBNET_ID=replace-with-approved-vip-subnet-id
export GATEWAY_OPENSTACK_E2E_EXPECTED_MEMBER_SUBNET_ID=replace-with-workload-member-subnet-id
export GATEWAY_OPENSTACK_E2E_EXPECTED_EXTERNAL_NETWORK_ID=none
export GATEWAY_OPENSTACK_E2E_CONTROLLER_CLOUDS_SECRET=openstack-clouds

export GATEWAY_OPENSTACK_E2E_AUDIT=true
export GATEWAY_OPENSTACK_E2E_CLUSTER_ID="gao-e2e-$RUN_ID"
export GATEWAY_OPENSTACK_E2E_AUDIT_CLOUDS_YAML=/absolute/path/to/clouds.yaml
export GATEWAY_OPENSTACK_E2E_AUDIT_CLOUD=openstack-e2e
export GATEWAY_OPENSTACK_E2E_AUDIT_REGION=RegionOne
export GATEWAY_OPENSTACK_E2E_AUDIT_OCTAVIA_MICROVERSION=2.5
```

The controller name must contain one controlled domain followed by the exact path `gao-e2e-<run-id>`.
The cluster ID must be exactly `gao-e2e-<run-id>`, and the controller namespace must be exactly `openstack-gateway-e2e-<run-id>`.

Use an OpenStack network ID instead of `none` when the run should allocate a Floating IP.
The external network variable is always explicit in shared mode, including when no external network is used.
Set the kubeconfig, image, source revision, backend image, and artifact variables from the common table before running the target.
The audit clouds.yaml path must be absolute, and the audit cloud and region must both be explicit.

The controller Secret must contain a self-contained application credential entry.
It must keep TLS verification enabled and must not inherit authentication from a profile or another cloud entry.
The `project_id` field may be omitted.
The preflight authenticates the credential and compares the project in the issued token with `GATEWAY_OPENSTACK_E2E_EXPECTED_PROJECT_ID`.

The audit may use a different credential.
The OpenStack operator should grant it only the authentication and inventory read permissions required by the audit.
Its local configuration may use the normal `clouds.yaml` and `secure.yaml` resolution supported by the OpenStack client.
The harness authenticates that credential separately and requires its token to resolve to the same expected project.
It does not inspect Keystone roles or policy and cannot prove that the assigned permissions are read-only.
The controller ConfigMap and audit settings still use the same cloud entry name and region so the selected environment is unambiguous.

The controller Secret can use an entry like this:

```yaml
clouds:
  openstack-e2e:
    auth_type: v3applicationcredential
    auth:
      auth_url: https://identity.example/v3
      application_credential_id: replace-me
      application_credential_secret: replace-me
    region_name: RegionOne
```

Create a controller overlay for the run instead of editing or restarting an existing controller.
Give every namespaced RBAC object, ServiceAccount, RoleBinding, ConfigMap, Secret, Lease, and controller Namespace a run-scoped name or namespace.
Keep cluster-scoped RBAC names unique as well.
Do not apply an overlay that replaces an ambient installation with the same object names.
The harness deletes the current leader Pod and later scales its configured Deployment to zero, so selecting an existing controller is not safe.
Inspect the rendered overlay because the harness cannot prove that cluster-scoped RBAC names are unique.

The controller Namespace, Deployment, ConfigMap, and clouds Secret must carry this annotation:

```yaml
metadata:
  annotations:
    e2e.gateway-api-openstack.io/run-id: leaf-e2e-20260828a
```

Set `immutable: true` on the ConfigMap and Secret.
The controller container must leave `command` empty so the image entrypoint starts the controller.
It must use exactly one unprefixed ConfigMap through `envFrom`.
Do not override a protected controller or OpenStack setting with a direct environment variable or command line flag.
Mount the named Secret read-only at `/etc/openstack` and map `clouds.yaml` to `/etc/openstack/clouds.yaml` exactly once.
The Secret volume may also project a CA certificate, client certificate, and client key referenced by the cloud entry.
Every referenced file must remain below `/etc/openstack` and come from the same immutable Secret.

The run ConfigMap must contain these exact values:

```yaml
data:
  GATEWAY_OPENSTACK_CONTROLLER_NAME: example.test/gao-e2e-leaf-e2e-20260828a
  GATEWAY_OPENSTACK_CLUSTER_ID: gao-e2e-leaf-e2e-20260828a
  GATEWAY_OPENSTACK_OCTAVIA_PROVIDER: amphora
  GATEWAY_OPENSTACK_MEMBER_MODE: NodePort
  GATEWAY_OPENSTACK_NODE_ADDRESS_TYPE: InternalIP
  GATEWAY_OPENSTACK_VIP_SUBNET_ID: replace-with-approved-vip-subnet-id
  GATEWAY_OPENSTACK_MEMBER_SUBNET_ID: replace-with-workload-member-subnet-id
  GATEWAY_OPENSTACK_EXTERNAL_NETWORK_ID: ""
  OS_CLIENT_CONFIG_FILE: /etc/openstack/clouds.yaml
  OS_CLOUD: openstack-e2e
  OS_REGION_NAME: RegionOne
```

When an external network is selected, put its ID in the ConfigMap and in `GATEWAY_OPENSTACK_E2E_EXPECTED_EXTERNAL_NETWORK_ID`.
The Deployment must set `--octavia-microversion=2.5` exactly once, using the same value as the audit.

Bind the Deployment Pod template to the final ConfigMap and Secret API versions.
Capture each immutable object's UID and `resourceVersion`, then set both source annotations:

```yaml
spec:
  template:
    metadata:
      annotations:
        e2e.gateway-api-openstack.io/controller-config-source: "<ConfigMap UID>/<resourceVersion>"
        e2e.gateway-api-openstack.io/controller-clouds-source: "<Secret UID>/<resourceVersion>"
```

The following commands print only the required object metadata and patch those values.
Replace the ConfigMap name if the overlay changed it:

```sh
CONTROLLER_CONFIG_MAP=openstack-gateway-controller-config
CONTROLLER_SECRET="$GATEWAY_OPENSTACK_E2E_CONTROLLER_CLOUDS_SECRET"

CONFIG_SOURCE="$(
  kubectl --kubeconfig="$GATEWAY_OPENSTACK_E2E_KUBECONFIG" \
    --context="$GATEWAY_OPENSTACK_E2E_CONTEXT" \
    -n "$GATEWAY_OPENSTACK_E2E_CONTROLLER_NAMESPACE" \
    get configmap "$CONTROLLER_CONFIG_MAP" \
    -o jsonpath='{.metadata.uid}/{.metadata.resourceVersion}'
)"
SECRET_SOURCE="$(
  kubectl --kubeconfig="$GATEWAY_OPENSTACK_E2E_KUBECONFIG" \
    --context="$GATEWAY_OPENSTACK_E2E_CONTEXT" \
    -n "$GATEWAY_OPENSTACK_E2E_CONTROLLER_NAMESPACE" \
    get secret "$CONTROLLER_SECRET" \
    -o jsonpath='{.metadata.uid}/{.metadata.resourceVersion}'
)"

kubectl --kubeconfig="$GATEWAY_OPENSTACK_E2E_KUBECONFIG" \
  --context="$GATEWAY_OPENSTACK_E2E_CONTEXT" \
  -n "$GATEWAY_OPENSTACK_E2E_CONTROLLER_NAMESPACE" \
  patch deployment "$GATEWAY_OPENSTACK_E2E_CONTROLLER_DEPLOYMENT" \
  --type=merge \
  -p "{\"spec\":{\"template\":{\"metadata\":{\"annotations\":{\"e2e.gateway-api-openstack.io/controller-config-source\":\"$CONFIG_SOURCE\",\"e2e.gateway-api-openstack.io/controller-clouds-source\":\"$SECRET_SOURCE\"}}}}}"

kubectl --kubeconfig="$GATEWAY_OPENSTACK_E2E_KUBECONFIG" \
  --context="$GATEWAY_OPENSTACK_E2E_CONTEXT" \
  -n "$GATEWAY_OPENSTACK_E2E_CONTROLLER_NAMESPACE" \
  rollout status \
  deployment/"$GATEWAY_OPENSTACK_E2E_CONTROLLER_DEPLOYMENT"
```

If either immutable object is recreated or its metadata changes, capture the new source value, patch the Pod template again, and wait for the new rollout to finish before running E2E.
The preflight rejects a Deployment whose annotations refer to an earlier source version.

Before creating a test object, shared mode verifies all of the following:

- The authenticated project is the exact expected project.
- The workload Namespace and generated `gao-e2e-<run-id>` GatewayClass do not already exist.
- No existing GatewayClass selects the run's controller name.
- The controller Namespace, Deployment, ConfigMap, and Secret carry the exact run annotation and have the expected immutable identity.
- The ConfigMap and Secret are immutable and the Deployment consumes only the approved configuration and credential sources.
- The Pod template records the exact ConfigMap and Secret UID and `resourceVersion`, and the resulting Deployment rollout is complete.
- The provider, networks, member mode, Node address type, cloud, region, and Octavia microversion match the approved values.
- The container uses the image entrypoint without a `command` override.
- The controller and audit credentials each authenticate to the exact expected project.
- The exact `controllerName` and cluster ID ownership audit scope is empty.

These checks reduce the chance of targeting an existing controller or graph.
They cannot stop a project-scoped credential from reaching unrelated resources.
Keep the independent, redacted project-wide inventory for the audit and cleanup steps below.

After the rollout above completes and the starting inventory is saved, run the same target as dedicated mode:

```sh
make E2E_ARTIFACT_DIR="/absolute/path/to/e2e-artifacts/$RUN_ID" test-e2e
```

Without an `E2E_ARTIFACT_DIR` override, the Make target uses the absolute path `_artifacts/e2e/<run-id>` below the repository.
Run `make test-e2e-compile` to compile the tagged suite without contacting a Kubernetes or OpenStack API.
Run `make test-e2e-unit` to execute the tagged configuration and preflight helper tests without contacting either API.
The repository's `make verify` target includes these helper tests.

`GATEWAY_OPENSTACK_E2E_RESTART_MODE` may be omitted or set to `cold`; no other restart mode is accepted.
The optional timing variables are `GATEWAY_OPENSTACK_E2E_TIMEOUT`, `GATEWAY_OPENSTACK_E2E_POLL_INTERVAL`, `GATEWAY_OPENSTACK_E2E_HTTP_TIMEOUT`, and `GATEWAY_OPENSTACK_E2E_NOOP_WINDOW`.
Their defaults are `45m`, `5s`, `10s`, and `30s` respectively.
The suite timeout may not exceed `50m`.
The default `45m` covers the main scenario run.
The Make target gives the Go test `90m` so that a timed-out run still has up to `15m` for emergency cleanup, another `15m` for the post-cleanup audit, and time to write the redacted artifact.

The suite starts the backend with `agnhost netexec --http-port=8080`.
Use the digest of the exact `agnhost` artifact available to the test cluster.

The harness reads metrics from the current leader without a separate metrics URL.
For each snapshot, it reads the configured Lease, matches its holder to a Ready Pod owned by the run-scoped Deployment, and opens that Pod's named `metrics` port at TCP 8080 through the Kubernetes API `pods/proxy` subresource.
The Deployment must expose exactly that named container port, and the kubeconfig identity must be allowed to get `pods/proxy`.
A metrics Service or a local port-forward is not required.

For the converged no-op assertion, `gateway_api_openstack_openstack_mutations_total` must not increase during the observation window, and the aggregate `gateway_api_openstack_openstack_mutations_in_flight` value must be zero in both snapshots.
A nonzero in-flight value means the window did not bound a complete set of mutation attempts and is not valid no-op evidence.
The `service="octavia",method="GET"` request counter and matching duration count must increase, showing that the level-based controller made a fresh, read-only Octavia observation after convergence.

The mutation counter is incremented when a potentially modifying request starts, before the HTTP round trip.
It uses only bounded `service` and `method` labels.
Requests other than GET, HEAD, and OPTIONS count conservatively unless their endpoint is positively identified as Keystone.

The harness aggregates every `service` label value in the mutation counter.
Modifying requests classified as `unknown` still count, so an endpoint catalog ambiguity cannot make the no-op check report a false zero.

The no-op check does not rely on the normal ten-minute resync happening during the short observation window.
After the first metrics snapshot, the harness changes the HTTPRoute backendRef namespace from omitted to the explicit test namespace.
Those forms have the same Gateway API meaning and compile to the same OpenStack graph.
The harness waits for status to observe the new generation before taking the second snapshot.
The request and duration counts must increase while the mutation attempt count stays unchanged.

The OpenStack counters begin after service discovery and reset when the controller process restarts.
Take both no-op snapshots from the current leader process after restart recovery has converged.
The harness checks the exact `leader_election_master_status` Lease label, `process_start_time_seconds`, the leader Pod UID, the controller container start time and restart count, and the Lease holder and transition count around the window.
It rejects a leader or process change between snapshots.
Do not compare a counter from before a cold restart with one from the new process.

The read-only ownership audit is opt-in in dedicated mode and required in shared mode.
Set `GATEWAY_OPENSTACK_E2E_AUDIT=true`, `GATEWAY_OPENSTACK_E2E_CLUSTER_ID`, and the audit authentication settings for the same project to collect aggregate counts before, during, and after the scenario.
The Make target builds the audit command and sets `GATEWAY_OPENSTACK_E2E_AUDIT_BINARY` to its absolute path.
When a clouds.yaml file is used, `GATEWAY_OPENSTACK_E2E_AUDIT_CLOUDS_YAML` must be an absolute path and `GATEWAY_OPENSTACK_E2E_AUDIT_CLOUD` must name its entry.
Region and Octavia microversion overrides use `GATEWAY_OPENSTACK_E2E_AUDIT_REGION` and `GATEWAY_OPENSTACK_E2E_AUDIT_OCTAVIA_MICROVERSION`; the microversion defaults to `2.5`.
Leaving the audit disabled keeps the required baseline-return check `Not run` and prevents an overall `Passed` result.

The preflight audit must complete with no findings and an empty summary for the exact controller name and cluster ID.
Once the Gateway graph is active, the audit must observe the two durable Kubernetes bindings and only matched OpenStack resources.
The harness compares an in-memory fingerprint of that owned inventory after leader recovery and after the cold restart.
It does not write resource or object identifiers to the artifact.
After cleanup, the same audit must return to the empty baseline.
A binding or OpenStack resource in that exact run scope makes the environment unsuitable for the run.
OpenStack resources tagged for another controller or cluster are not part of this comparison.
A Kubernetes object that carries the run controller's binding but has contradictory identity still fails the audit.

This comparison covers only resources visible to the ownership audit at each snapshot.
It cannot find a resource after all controller scope metadata is removed, attribute an unmatched detached Floating IP, prove that the desired graph is complete, or rule out a transient duplicate between snapshots.
See the [ownership audit contract](design/ownership-audit.md) for the full limits.
Use a separate, redacted project-wide inventory before and after the run to make a leak claim.
In shared mode, review and attribute the difference instead of expecting unrelated concurrent work to disappear.

## Run the baseline scenarios

Start with a clean project in dedicated mode, or an empty exact audit scope in shared mode, and keep the same controller image digest for the whole run.
Capture the following scenarios separately:

1. Verify that GatewayClass, Gateway, listener, and HTTPRoute conditions reach the expected state.
2. Send HTTP traffic through the address published in Gateway status and record the response.
3. Delete the current leader Pod and verify that another replica acquires the Lease, traffic recovers, and the active ownership inventory is unchanged.
4. Perform the harness's cold controller restart after convergence and verify that traffic remains available while the controller is stopped and that the same active ownership inventory is restored.
5. After restart recovery converges, apply the harness's equivalent backendRef namespace form and verify that its fresh OpenStack observation makes no mutation.
6. Delete the HTTPRoute, Gateway, GatewayClass, and test Namespace in dependency order and wait for finalization to complete.
7. Run the ownership audit and a redacted OpenStack inventory.
   Verify that the resources created for the test are absent.

The foundations harness uses `externalTrafficPolicy: Cluster` but does not exercise member changes.
Test changes in `Cluster` mode and `externalTrafficPolicy: Local` as separate follow-up cases.
Record the Node and EndpointSlice change made in each case and the resulting Octavia member set.

## Run fault scenarios deliberately

Quota, timeout, Octavia failure, lost response, and blocked finalization tests need a repeatable and safe fault mechanism.
Run only the cases for which the environment owner has approved that mechanism.
Do not run these fault cases in shared mode.
Do not exhaust a shared quota, modify another tenant's resources, or tamper with identity metadata to produce a result.

The foundations harness does not inject a partial OpenStack transition.
Keep that report row as `Not run` unless a separate, reviewed mechanism creates a repeatable checkpoint without weakening the ownership checks.

Leave a fault row as `Not run` when the mechanism is unavailable or unsafe.
Unit or adapter coverage does not turn an OpenStack E2E row into a pass.
If a cloud service or the test environment fails before the assertion can be made, record an environment failure in the notes and keep the scenario from being reported as passed.

For blocked finalization, retain the finalizer and follow the [operator recovery guide](operator-recovery.md).
Never remove a controller finalizer by hand as part of the test.

## Record the result

Copy the [report template](reports/openstack-e2e-template.md) into a directory named for the controller release or short commit.
Fill in the exact environment and artifact fields before changing any result from `Not run`.

Use these result meanings:

- `Passed` means the named assertion was observed and the report points to retained, redacted evidence.
- `Failed` means the test ran and the expected controller behavior was not observed.
- `Skipped` means an optional check was outside the configured run.
  It does not provide evidence for that check.
- `Not run` means the scenario was not attempted or did not reach the point where the assertion could be evaluated.

The harness writes `report.json` and `report.md` into a new artifact directory and refuses to overwrite an existing run.
Its overall `Passed` value requires every foundations check, including the metrics no-op check and the post-test ownership audit baseline return, to pass.
Metrics and audit configuration remain opt-in safeguards, but leaving either one out keeps the overall artifact `Not run` and incomplete as Phase 2 evidence.
Unexecuted fault scenarios do not block the foundations result and remain `Not run`.
Read the individual rows; the overall value does not replace them.

The generated files are local supporting output.
They record the source revision, controller image digest, Gateway API bundle, project mode, restart mode, and check summaries.
They never record expected project, subnet, or network IDs, or the ConfigMap and Secret source annotation values.
The files do not replace the report template's environment, topology, independent project-wide inventory, and publication review.
A `Passed` generated artifact therefore covers only the automated foundations checks; it is not a complete Phase 2 evidence report by itself.
Copy only redacted evidence into the durable report.

A retry may be useful while diagnosing a problem, but it does not erase the first unexplained result.
Record both attempts and link a follow-up issue when the cause remains unknown.

## Clean up safely

Delete the test HTTPRoute and Gateway before removing the controller, credentials, or Gateway API CRDs.
Wait for finalization.
If cleanup stops, leave the controller and credentials available and use the read-only ownership audit to diagnose it.

After Kubernetes cleanup completes, compare an independent, project-wide final inventory with the starting inventory.
Report every remaining resource that can be tied to the run.
In a shared project, have the project users attribute concurrent changes and do not treat an unexplained difference as a test pass.
The scoped ownership audit baseline check does not replace this comparison.
Do not delete an unexpected resource unless its complete immutable identity and surrounding graph have been validated.

Redact the final report once more before publishing it.
In particular, check command output, Events, object YAML, OpenStack request logs, and links to CI artifacts for credentials, tokens, project and resource IDs, private addresses, certificates, Pod names and UIDs, leader Lease holder identities, and customer details.
