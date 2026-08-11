# Getting started with the current controller

The controller is pre-alpha. Phase 1 is implemented, and Phase 2 focuses on
reliability. The Phase 0 probe tested the required Octavia and Neutron
operations in one environment. The project has not yet published results from
end-to-end controller testing in an OpenStack environment. No release image is
available.

Use these manifests only in a disposable test project. Keep
cloud-provider-openstack responsible for Services of type `LoadBalancer`. This
controller creates separate resources only for its Gateway API objects.

## Prerequisites

- A Kubernetes cluster with the Gateway API Standard Channel v1.6.1 CRDs.
- An OpenStack project with Octavia and Neutron access. The controller accepts
  only Amphora. Support claims are limited to environment profiles with
  published compatibility evidence.
- A Keystone application credential with only the permissions that the
  controller needs, stored in a named `clouds.yaml` entry.
- A VIP subnet and a member subnet from which Amphora can reach the selected
  Kubernetes Node addresses and allocated NodePorts.
- Security groups and routing that allow Amphora health checks and HTTP
  traffic to those Node addresses and NodePorts.
- Optionally, an external network for allocating a Floating IP. Without one,
  Gateway status reports the Octavia VIP, which may be reachable only from
  private networks.
- A controller container image built from the source revision being deployed.
  The repository does not currently publish a supported image.

The member subnet ID is always required. `InternalIP` is the default Node
address type. Select `ExternalIP` only when those addresses belong to the
configured member network and are reachable from Amphora.

The current controller does not create networks, subnets, routers, or backend
security groups and does not attach security groups to worker ports. Operators
must establish that connectivity before creating a Gateway. Planned automation
will not take ownership of existing networks, subnets, routers, or worker
ports. Any future change to a worker port's security groups will require an
explicit opt-in and the ownership safeguards described in the roadmap.

Install the pinned Standard Channel CRDs if the cluster does not already have
them:

```sh
kubectl apply -f \
  https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.1/standard-install.yaml
```

The controller also performs a startup discovery check and exits with a clear
error when GatewayClass, Gateway, or HTTPRoute is unavailable.

## Prepare OpenStack credentials

Create a local `clouds.yaml` containing an application credential. Do not add
this file to the repository.

```yaml
clouds:
  openstack:
    auth:
      auth_url: https://identity.example/v3
      application_credential_id: replace-me
      application_credential_secret: replace-me
    auth_type: v3applicationcredential
    region_name: RegionOne
    interface: public
    identity_api_version: 3
```

Create the controller namespace, then store the file as a Secret. The Secret
name and key match the Deployment volume in `config/manager/deployment.yaml`.

```sh
kubectl apply -f config/default/namespace.yaml
kubectl -n openstack-gateway-system create secret generic openstack-clouds \
  --from-file=clouds.yaml=/absolute/path/to/clouds.yaml
```

The Secret is mounted read-only and credentials are not copied into
ConfigMaps, Gateway API status, or examples.

## Configure and install

Edit `config/manager/controller-config.yaml` before applying it:

- Replace the example controller name with one under a domain controlled by the
  operator.
- Set the cluster ID to a value that is unique to this cluster, and do not
  change it while managed resources exist.
- Set the VIP and member subnet IDs.
- Set the external network ID if the controller should allocate Floating IPs.
- Set `OS_CLOUD` to the entry in the mounted `clouds.yaml`.
- Set `OS_REGION_NAME` only when it should override `region_name` in that file.

The same controller name must be used in every managed
`GatewayClass.spec.controllerName`, including `examples/basic.yaml`.

Build and push an image from the exact source revision you are deploying, then
edit the image override in `config/default/kustomization.yaml`. The image
reference in the repository uses `example.invalid` and is intentionally
unusable, so it cannot be mistaken for a published release.

```sh
make container-build \
  IMAGE=registry.example/openstack-gateway-controller:v0.1.0-dev \
  VERSION=v0.1.0-dev
docker push registry.example/openstack-gateway-controller:v0.1.0-dev
```

Set `CONTAINER_TOOL=podman` on the `make` command when Podman is preferred.

Render and inspect the resources, then install them:

```sh
kubectl kustomize config/default
kubectl apply -k config/default
kubectl -n openstack-gateway-system rollout status \
  deployment/openstack-gateway-controller
```

The Deployment starts two replicas with leader election. It exposes health and
readiness probes on port 8081 and controller-runtime metrics inside each Pod on
port 8080. Metrics are not exposed through a Service by the base manifests.

## Current configuration reference

The ConfigMap contains no secrets. It uses the environment variables accepted
by the controller binary.

| Setting | Environment variable | Current behavior |
| --- | --- | --- |
| Controller identity | `GATEWAY_OPENSTACK_CONTROLLER_NAME` | Required. Must exactly match GatewayClass |
| Cluster identity | `GATEWAY_OPENSTACK_CLUSTER_ID` | Required and stable |
| Octavia provider | `GATEWAY_OPENSTACK_OCTAVIA_PROVIDER` | Required and fixed to `amphora`. Other providers are not supported |
| VIP subnet | `GATEWAY_OPENSTACK_VIP_SUBNET_ID` | Required |
| External network | `GATEWAY_OPENSTACK_EXTERNAL_NETWORK_ID` | Optional Floating IP allocation |
| Member subnet | `GATEWAY_OPENSTACK_MEMBER_SUBNET_ID` | Required. Contains selected Node addresses |
| Member mode | `GATEWAY_OPENSTACK_MEMBER_MODE` | Must be `NodePort` |
| Node address | `GATEWAY_OPENSTACK_NODE_ADDRESS_TYPE` | `InternalIP` or `ExternalIP` |
| Monitor path | `GATEWAY_OPENSTACK_HEALTH_PATH` | Absolute HTTP path. Defaults to `/` |
| Cloud name | `OS_CLOUD` | Named entry in `clouds.yaml` |
| Cloud file | `OS_CLIENT_CONFIG_FILE` | Mounted `/etc/openstack/clouds.yaml` |
| Region | `OS_REGION_NAME` | Optional override |

The base Deployment also sets these binary flags: leader election enabled,
metrics on `:8080`, probes on `:8081`, and Octavia microversion `2.5`. The
binary additionally supports `--openstack-operation-timeout` and
`--openstack-poll-interval`. The manifests do not override these two flags, so
the binary uses defaults of 10 minutes and 2 seconds. The timeout limits each
provider reconciliation call, including all OpenStack requests made by that
call. After starting an asynchronous Octavia change, the controller returns and
observes it again after the poll interval instead of waiting for it to finish
in the same reconciliation.

## Apply the basic example

Ensure the controller name in `examples/basic.yaml` matches the ConfigMap, then
apply it:

```sh
kubectl apply -f examples/basic.yaml
kubectl get gatewayclass openstack -o yaml
kubectl -n gateway-api-openstack-demo get gateway edge -o yaml
kubectl -n gateway-api-openstack-demo get httproute basic -o yaml
```

The example intentionally uses one Gateway, one HTTP listener, one HTTPRoute,
one rule, one PathPrefix match, and one NodePort Service backend. Wait until the
Gateway's `Accepted` and `Programmed` conditions are `True`. In the HTTPRoute
parent status, wait for `Accepted`, `ResolvedRefs`, and the
`<controller-domain>/Programmed` condition defined by this controller to become
`True`. The address is published at `Gateway.status.addresses[0].value`.

```sh
kubectl -n gateway-api-openstack-demo get gateway edge \
  -o jsonpath='{.status.addresses[0].value}{"\n"}'
```

Send an HTTP request to that address from a network that can reach it. A VIP
without a Floating IP may not be reachable from the public Internet.

## Current implementation limitations

- HTTP is the only listener protocol. TLS is not supported.
- Each Gateway has exactly one listener and one selected HTTPRoute in the same
  namespace.
- Each HTTPRoute has one rule, at most one exact hostname, at most one Exact or
  PathPrefix match, and one Service backend in the same namespace.
- Backend Services must be `type: NodePort`. Both `externalTrafficPolicy:
  Cluster` and `Local` are handled, but Local requires ready EndpointSlices to
  identify their Nodes.
- Filters, headers, query parameters, methods, regular expressions, backend
  weights of zero, cross-namespace references, and `parametersRef` are rejected
  in status.
- Existing OpenStack resources are never adopted.

## Safe removal

Keep the controller, its application credential, and OpenStack access running
until every managed HTTPRoute and Gateway has finished finalization. Removing
the Deployment or Secret first can leave OpenStack resources behind and leave
Kubernetes objects stuck in deletion.

For the basic example, delete in this order:

```sh
kubectl -n gateway-api-openstack-demo delete httproute basic
kubectl -n gateway-api-openstack-demo delete gateway edge
kubectl delete gatewayclass openstack
kubectl delete -f examples/basic.yaml --ignore-not-found
kubectl delete -k config/default
```

Each of the first three commands waits for deletion by default. If one remains
blocked, inspect its conditions, controller logs, and the matching OpenStack
resources. Do not remove controller finalizers manually because doing so
bypasses ownership checks and cleanup.

Before uninstalling a shared controller, repeat the HTTPRoute, Gateway, and
GatewayClass deletion sequence for every class using its exact controller
name. The final `kubectl delete -k` removes the dedicated
`openstack-gateway-system` namespace and its credential Secret. It does not and
remove the cluster-scoped Gateway API CRDs, which may be shared by other
controllers.
