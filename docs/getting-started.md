# Getting started with the Phase 1 controller

The controller is pre-alpha and Phase 1 is still in progress. The retained
capability probe has exercised the required Octavia and Neutron primitives in
one OpenStack environment. The complete Kubernetes controller path has not yet
passed a published real-cloud end-to-end suite, and no release image is
currently provided.

Use these manifests only in a disposable test project. Keep
cloud-provider-openstack responsible for `Service type=LoadBalancer`; this
controller creates separate resources only for its Gateway API objects.

## Prerequisites

- A Kubernetes cluster with the Gateway API Standard Channel v1.6.1 CRDs.
- An OpenStack project with Octavia and Neutron access. Phase 1 accepts only
  the Amphora provider.
- A least-privilege Keystone application credential stored in a named
  `clouds.yaml` entry.
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
address type; select `ExternalIP` only when those addresses belong to the
configured member network and are reachable from Amphora.

Install the pinned Standard Channel CRDs if the cluster does not already have
them:

```sh
kubectl apply -f \
  https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.1/standard-install.yaml
```

The controller also performs a startup discovery check and exits with a clear
error when `GatewayClass`, `Gateway`, or `HTTPRoute` is unavailable.

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

- replace the example controller name with a domain-prefixed name controlled
  by the operator;
- set a stable, cluster-unique ID and never change it while managed resources
  exist;
- set the VIP and member subnet IDs;
- optionally set the external network ID;
- set `OS_CLOUD` to the entry in the mounted `clouds.yaml`; and
- set `OS_REGION_NAME` only when it should override `region_name` in that file.

The same controller name must be used in every managed
`GatewayClass.spec.controllerName`, including `examples/basic.yaml`.

Build and push an image from the exact source revision you are deploying, then
edit the image override in `config/default/kustomization.yaml`. The checked-in
`example.invalid` image is intentionally unusable so an unpublished image is
never mistaken for a release artifact.

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

## Configuration reference

The non-secret ConfigMap uses the environment variables accepted by the
controller binary.

| Setting | Environment variable | Phase 1 behavior |
| --- | --- | --- |
| Controller identity | `GATEWAY_OPENSTACK_CONTROLLER_NAME` | Required; must exactly match GatewayClass |
| Cluster identity | `GATEWAY_OPENSTACK_CLUSTER_ID` | Required and stable |
| Octavia provider | `GATEWAY_OPENSTACK_OCTAVIA_PROVIDER` | Must be `amphora` |
| VIP subnet | `GATEWAY_OPENSTACK_VIP_SUBNET_ID` | Required |
| External network | `GATEWAY_OPENSTACK_EXTERNAL_NETWORK_ID` | Optional Floating IP allocation |
| Member subnet | `GATEWAY_OPENSTACK_MEMBER_SUBNET_ID` | Required; contains selected Node addresses |
| Member mode | `GATEWAY_OPENSTACK_MEMBER_MODE` | Must be `NodePort` |
| Node address | `GATEWAY_OPENSTACK_NODE_ADDRESS_TYPE` | `InternalIP` or `ExternalIP` |
| Monitor path | `GATEWAY_OPENSTACK_HEALTH_PATH` | Absolute HTTP path; defaults to `/` |
| Cloud name | `OS_CLOUD` | Named entry in `clouds.yaml` |
| Cloud file | `OS_CLIENT_CONFIG_FILE` | Mounted `/etc/openstack/clouds.yaml` |
| Region | `OS_REGION_NAME` | Optional override |

The base Deployment also sets these binary flags: leader election enabled,
metrics on `:8080`, probes on `:8081`, and Octavia microversion `2.5`. The
binary additionally supports `--openstack-operation-timeout` and
`--openstack-poll-interval`; the manifests retain their defaults of 10 minutes
and 2 seconds.

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
one rule, one path-prefix match, and one NodePort Service backend. Wait for the
Gateway and HTTPRoute `Accepted` and `Programmed` conditions to become `True`.
The address is published at `Gateway.status.addresses[0].value`.

```sh
kubectl -n gateway-api-openstack-demo get gateway edge \
  -o jsonpath='{.status.addresses[0].value}{"\n"}'
```

Send an HTTP request to that address from a network that can reach it. A VIP
without a Floating IP may not be reachable from the public Internet.

## Phase 1 limitations

- HTTP is the only listener protocol; TLS is not supported.
- Each Gateway has exactly one listener and one selected same-namespace
  HTTPRoute.
- Each HTTPRoute has one rule, at most one exact hostname, at most one Exact or
  PathPrefix match, and one same-namespace Service backend.
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
the Deployment or Secret first can strand OpenStack resources behind blocked
Kubernetes finalizers.

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
resources. Do not force-remove controller finalizers: that bypasses ownership
checks and cleanup.

Before uninstalling a shared controller, repeat the Route, Gateway, and
GatewayClass deletion sequence for every class using its exact controller
name. The final `kubectl delete -k` removes the dedicated
`openstack-gateway-system` namespace and its credential Secret. It does not and
should not remove the cluster-wide Gateway API CRDs.
