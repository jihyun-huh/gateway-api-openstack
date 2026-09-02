# Run the OpenStack controller E2E checks

This guide covers the first end-to-end checks for the current HTTP and NodePort slice.
The checks use a real Kubernetes API server and an OpenStack Amphora data plane.
They do not run the Gateway API conformance suite and they do not establish general OpenStack compatibility.

No completed controller E2E report has been published yet.
A local run becomes evidence only after its environment, immutable artifacts, results, and redacted supporting records are captured in an [OpenStack E2E report](reports/openstack-e2e-template.md).

## Before you start

Use a Kubernetes cluster reserved for this test.
The runner deletes a leader Pod, scales its controller Deployment to zero during recovery testing, and creates cluster-scoped RBAC with names unique to the run.
The `kubernetes.dedicatedForE2E` setting is an operator acknowledgement.
The runner cannot prove that no one else uses the cluster.

Prepare all of the following:

- Gateway API v1.6.1 Standard Channel CRDs on the cluster.
- A kubeconfig and explicit context that can create, inspect, update, and delete the run-scoped workloads, controller objects, and cluster-scoped RBAC.
  The test gets the run-scoped Namespaces, ConfigMaps, and Secrets, lists GatewayClasses, and reads Gateway API CRDs, Gateways, HTTPRoutes, EndpointSlices, ReplicaSets, Pods, and the leader Lease.
  It deletes the current leader Pod, updates the controller Deployment, uses the `pods/proxy` subresource for metrics, and lists Gateways and HTTPRoutes for the audit.
- An OpenStack project with Amphora and enough quota for the Gateway graph and an optional Floating IP.
- A controller image built from the revision under test and an `agnhost` backend image, both available to the cluster and pinned by digest.
- An approved VIP subnet, member subnet, and explicit choice about using an external network.
- A path from Amphora to the selected Node `InternalIP` addresses and NodePorts.
- A path from the machine running the command to the Keystone `auth_url` used by both credential configurations and to the resulting VIP or Floating IP.
- A redacted project-wide inventory recorded before the run.

Do not put a populated `clouds.yaml`, application credential, token, kubeconfig, certificate, or unredacted inventory in the repository or command output.

## Prepare one configuration file

Copy the synthetic example to a private path:

```sh
mkdir -p _artifacts/e2e-config
cp test/e2e/openstack.example.yaml \
  _artifacts/e2e-config/openstack.yaml
```

Replace every placeholder before running the test.
The parser rejects unknown fields, and every path value in the file must be absolute.
Apart from the acknowledgement selected by the project mode, the only optional settings are `runID`, `openstack.auditCloudsYAML`, and `artifacts`.

### Choose the OpenStack project mode

A disposable, dedicated OpenStack project is preferred and is expected for release evidence.
Keep this pair when the project is reserved for the run and empty of unrelated resources:

```yaml
openstack:
  projectMode: dedicated
  dedicatedForE2E: true
```

Use this pair when other engineers share the OpenStack project:

```yaml
openstack:
  projectMode: shared
  acceptProjectWideCredentialRisk: true
```

The two OpenStack acknowledgements are mutually exclusive.
They state what the operator has checked and do not change Keystone permissions.
Both modes still require the dedicated Kubernetes cluster acknowledgement.

Shared mode narrows the controller and audit to a unique logical identity, but the credential can still consume project quota, use Octavia and Neutron API capacity, allocate a Floating IP, and reach anything its roles permit.
Do not run quota, fault injection, security group, or destructive recovery cases in a shared project.
Have the project users review and attribute the project-wide inventory difference after the run.

### Set the project and network inputs

Set `openstack.expectedProjectID` to the exact project UUID approved for the run.
The runner authenticates the controller and audit configurations independently before it creates the controller, and both token projects must match this value.
The generated artifacts and public report must not contain the project ID.

Set `openstack.cloud` to the selected cloud entry and `openstack.region` to the exact region under test.
The runner fixes the provider to Amphora, the member mode to NodePort, the Node address type to `InternalIP`, and the Octavia microversion to `2.5`.

Set `openstack.vipSubnetID` and `openstack.memberSubnetID` to the approved network IDs.
Set `openstack.externalNetworkID` to an exact external network ID when the test should allocate a Floating IP, or to the literal `none` when it should not.
The runner does not choose networks, reserve quota, or prove that the network path is safe.

### Set the credentials

`openstack.controllerCloudsYAML` must point to a file containing one self-contained cloud entry that uses an application credential.
TLS verification must remain enabled, so omit `verify` or set it to `true`.
The runner rejects profile inheritance, password or token authentication, `verify: false`, and references to additional CA, client certificate, or client key files.
It copies only the exact `clouds.yaml` bytes into the immutable controller Secret.
The `project_id` field may be omitted because the runner checks the project in the issued token.

The audit can use the controller credential in a dedicated evaluation project.
To use a separate credential, set `openstack.auditCloudsYAML` to its absolute path.
The audit file must follow the same one-entry, self-contained application credential and TLS verification rules because the runner authenticates its exact bytes before installation.
The OpenStack operator should grant this credential only the authentication and inventory permissions it needs.
The runner checks its project but cannot prove that its assigned roles are read-only.

### Pin the remaining inputs

Set `controller.domain` to a DNS domain you control.
Set `controller.image` to a complete image reference with a lowercase SHA-256 digest and set `controller.sourceRevision` to the full lowercase Git commit used to build it.
Set `backend.image` to a digest-pinned `agnhost` image.

Omit `runID` for normal runs so the runner generates a new DNS label.
An explicit `runID` must be between 8 and 32 characters and must not reuse a retained or previously recorded run.
Omit `artifacts` to write below `_artifacts/e2e/<run-id>`, or set `artifacts.root` to a non-root absolute directory.
The runner refuses to overwrite an existing run directory.

## Run the suite

Run the live suite with one command:

```sh
make test-e2e E2E_CONFIG="$PWD/_artifacts/e2e-config/openstack.yaml"
```

The target builds the ownership audit, compiles the runner and tagged E2E suite, installs a controller dedicated to the run, and executes the live checks.
The YAML file is the source of truth.
Inherited `GATEWAY_OPENSTACK_*` and `OS_*` variables do not override the generated test environment.

These local targets do not contact Kubernetes or OpenStack:

```sh
make test-e2e-unit
make test-e2e-compile
```

Live E2E remains opt-in and is not part of `make verify` or a scheduled workflow.

## What the runner checks

The runner performs these operations in order:

1. Validate the strict configuration, local files, image digests, source revision, kubeconfig, and artifact path.
2. Generate one run identity and derive the workload Namespace, GatewayClass, controller name, cluster ID, controller Namespace, and RBAC names.
3. Require the derived Namespaces, GatewayClass identity, ClusterRole, and ClusterRoleBinding to be absent.
4. Authenticate an isolated exact copy of the controller credential and the audit configuration against the expected project.
5. Run the ownership audit and require a complete, empty result for the exact controller name and cluster ID.
6. Create the Namespace, ServiceAccount, unique ClusterRole and ClusterRoleBinding, immutable ConfigMap and Secret, and controller Deployment with create-only writes.
7. Bind the Pod template to the ConfigMap and Secret UID and `resourceVersion`, then require two Ready controller Pods, the expected image digest, and a live leader Lease.
8. Run the foundations scenarios, ordered cleanup, and the final ownership audit.
9. Remove the run-scoped controller installation only after the workload graph is gone and the exact audit scope is empty again.

No controller object is created before both credentials and the empty audit baseline pass.
Names, tags, and the scoped audit are safety signals, not an OpenStack authorization boundary.

## Foundations scenarios

The automated run covers the following checks:

1. Validate the pinned Gateway API bundle and the run-scoped controller installation.
2. Create an isolated NodePort backend.
3. Wait for the expected GatewayClass, Gateway, listener, and HTTPRoute status.
4. Require the active ownership audit to find both durable bindings and only matched OpenStack resources.
5. Send HTTP traffic through the address published in Gateway status.
6. Delete the current leader Pod and verify recovery with the same ownership inventory.
7. Stop and restore the controller Deployment and verify cold restart recovery.
8. Trigger a semantically equivalent HTTPRoute update and verify a fresh Octavia observation with no OpenStack mutation.
9. Delete HTTPRoute, Gateway, GatewayClass, and the workload Namespace in dependency order and wait for finalization.
10. Require the scoped ownership audit to return to its empty baseline.

The no-op check reads metrics from the current leader through the Kubernetes API.
It requires the OpenStack mutation counter to remain unchanged and the Octavia GET observation counter to increase while the leader and controller process remain stable.

The foundations runner does not inject quota, timeout, Octavia failure, partial transition, blocked finalization, or external deletion faults.
Keep those rows `Not run` unless a separate, reviewed mechanism exercises them in a dedicated project.
Unit, adapter, or envtest coverage does not turn an OpenStack E2E row into a pass.

## Audit and evidence limits

The ownership audit is required in both project modes.
It observes resources in the exact controller name and cluster ID scope before, during, and after the scenario.
It does not prove that the desired graph is complete, find a detached resource after all scope metadata is lost, detect every transient duplicate, or replace a project-wide inventory.
An empty audit result never grants deletion or adoption authority.
See the [ownership audit contract](design/ownership-audit.md) for the full limits.

The runner writes `report.json` and `report.md` in a new artifact directory.
The artifacts record the source revision, controller image digest, Gateway API bundle, project mode, restart mode, fixed check summaries, and aggregate audit and metrics evidence.
They do not record project, subnet, network, Kubernetes object, or OpenStack resource IDs.

The overall result is `Passed` only when every foundations check, including active ownership evidence, finalization, and the post-test empty audit, passes.
A failed assertion is `Failed`.
An optional check outside the run is `Skipped`.
A scenario that was not attempted or did not reach its assertion remains `Not run`.

The generated artifact is supporting output, not a complete Phase 2 environment report.
Copy the [report template](reports/openstack-e2e-template.md), describe the environment without private identifiers, and link only retained redacted evidence.
A retry does not erase an unexplained earlier failure.

## Failure and cleanup

If local validation, project authentication, or the pre-install audit fails, the runner stops without installing the controller.
If installation fails before it attempts to create the Deployment, it removes a partial installation only when every object still has the UID and run annotation it recorded.
Once it attempts to create the Deployment, it does not run automatic controller cleanup after a create, rollout, suite, audit, or cancellation failure.
The remaining controller objects and clouds Secret stay available for investigation and finalization.

Do not remove the retained controller, revoke its credential, force a finalizer, or delete an OpenStack resource by name.
Follow the [operator recovery guide](operator-recovery.md) and use the read-only audit until the owned graph is confirmed absent.
Do not reuse the run ID while any retained object or report remains.

After a successful run, compare a redacted project-wide final inventory with the starting inventory.
In shared mode, have the project users attribute every concurrent difference.
The runner does not delete an OpenStack project, application credential, network, or unrelated resource.

Before publishing a report, remove credentials, tokens, project and resource IDs, private addresses, certificates, Pod names and UIDs, leader Lease holder identities, source annotation values, customer data, and unredacted command output.
