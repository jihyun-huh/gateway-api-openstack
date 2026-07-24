# Gateway API for OpenStack

> [!IMPORTANT]
> This project is in pre-alpha. The architecture is being validated and no
> production release is available yet.
>
> This repository does not yet contain a working controller.
> The architecture, API surface, project name, and roadmap may change
> as the initial prototype is implemented and evaluated.
>
> Do not use this project in production.

gateway-api-openstack is an experimental Kubernetes [Gateway API](https://gateway-api.sigs.k8s.io/)
implementation backed by OpenStack Octavia.

The project is centered on an Octavia-native controller. It reconciles
`GatewayClass`, `Gateway`, and `HTTPRoute` resources into Octavia load
balancers, listeners, pools, members, health monitors, and related Neutron
resources. It does this without taking ownership of
`Service` resources or load balancers managed by
[cloud-provider-openstack](https://github.com/kubernetes/cloud-provider-openstack).


## Why this project?

Kubernetes users on OpenStack currently have three separate concerns:

- cloud-provider-openstack provisions Octavia load balancers for
  `Service type=LoadBalancer`;
- proxy controllers such as Traefik implement Gateway API inside the cluster;
- Octavia exposes cloud-native L4 and selected L7 load-balancing capabilities,
  but there is no independent Gateway API controller that maps Gateway
  resources directly to those capabilities.

This project aims to close the third gap while retaining a clean ownership
boundary with existing cloud-provider components.

## Architecture

```mermaid
flowchart TD
    S["Service type=LoadBalancer"] --> OCCM["cloud-provider-openstack"]
    OCCM --> SLB["OCCM-owned Octavia LB"]

    G["Gateway API resources"] --> CTRL["openstack-gateway-controller"]
    CTRL --> OLB["Gateway-owned Octavia LB"]
    CTRL --> K8S["Service and EndpointSlice reads"]
```

The controller:

- watches only Gateway API resources assigned to its controller name;
- reads backend `Service`, `EndpointSlice`, `Secret`, and `ReferenceGrant`
  resources as required;
- **creates only Gateway-owned OpenStack resources**;
- never adopts, mutates, or deletes an OCCM-owned load balancer;
- records an immutable Kubernetes object UID and controller identity on every
  OpenStack resource where the API supports metadata or tags.

See [the architecture document](docs/design/architecture.md) for the proposed
resource mapping and ownership contract.

## Deployment paths

| Path | Gateway controller | Data plane | Purpose |
| --- | --- | --- | --- |
| Octavia native | This project | Octavia | Primary implementation; independent of the OCCM Service controller |
| Traefik reference profile | Traefik | Traefik behind an OpenStack L4 load balancer | Adopter-driven future work |
| Envoy reference profile | Not yet planned | Envoy | Adopter-driven future work |

These are separate deployment paths. The native controller does not manage
Traefik or Envoy, and the proxy-backed profiles do not share ownership of
occm loadbalancer controller(L4).

## Initial scope

The first native vertical slice targets:

- the Gateway controller profile;
- `GatewayClass`, `Gateway`, and `HTTPRoute`;
- HTTP listeners;
- hostname, exact-path, and path-prefix matching;
- a single Service backend per rule;
- one Octavia load balancer per Gateway;
- Octavia listeners, L7 policies and rules, pools, members, and health monitors;
- Floating IP allocation;
- standard Gateway API status conditions;
- the Octavia Amphora provider as the first tested provider.

The first release will not claim full Gateway API conformance. Octavia provider
capabilities vary, and some Gateway API HTTP features cannot be represented by
Octavia L7 policies. Unsupported features must be rejected explicitly in
resource status rather than ignored.

## Non-goals

- Replacing cloud-provider-openstack.
- Reimplementing the Kubernetes Service controller.
- Owning or migrating existing OCCM load balancers.
- Hiding provider capability differences.
- Supporting Traefik and Envoy inside the native reconciliation loop.
- Claiming that success in one OpenStack cloud proves compatibility with every
  OpenStack deployment.

## Project status

Current milestone: **Phase 0 — feasibility and ownership validation**.

The immediate work is to validate:

1. Octavia and Neutron resource mapping.
2. Backend reachability using NodePort and, where routable, Pod IP members.
3. Safe resource identity, deletion, and recovery.
4. The Gateway API conformance gap for the selected Octavia provider.
5. The reference deployment on an OpenStack Kubernetes environment.

See [ROADMAP.md](ROADMAP.md) for the complete phased plan.

## Community direction

This project is currently an independently maintained, experimental
open source project.

Its long-term goals are to:

- implement a conformant Gateway API controller backed by OpenStack Octavia;
- be listed as a Gateway API implementation after meeting the relevant requirements;
- build a diverse group of users, contributors, and maintainers; and
- explore an appropriate long-term community home if the project demonstrates
  sustained adoption and contributor interest.

These are project goals, not commitments or claims of upstream acceptance.

This project is not currently affiliated with or endorsed by the Kubernetes
project, SIG Network, the Gateway API project, the OpenInfra Foundation,
or the cloud-provider-openstack project.

## Contributing

Design feedback, OpenStack provider reports, and implementation contributions
are welcome. Start with [CONTRIBUTING.md](CONTRIBUTING.md) and the issues marked
`help wanted`.

Until the first architecture decision records are accepted, substantial API or
controller changes should begin as a design issue.

## Security

Please do not report vulnerabilities in a public issue. Follow
[SECURITY.md](SECURITY.md).

## License

Licensed under the [Apache License 2.0](LICENSE).

## Disclaimer

This is an independent community project. It is not currently an official
project of OpenInfra, OpenStack, Kubernetes, StackHPC, or their respective
foundations or organizations.
