# Support

gateway-api-openstack is pre-alpha. It does not have a supported production
release, a response-time commitment, or a published controller image. Use it
only in a disposable OpenStack project unless a later release says otherwise.

## Ask for help

Use a GitHub issue for a reproducible controller problem or a focused question.
Include the controller revision, Kubernetes and Gateway API versions, OpenStack
and Octavia releases, negotiated microversion, Amphora topology, CNI,
kube-proxy mode, and a redacted description of the network path. The issue
templates list the details that are normally needed.

Do not post credentials, tokens, project IDs, private addresses, customer data,
or raw provider responses. Follow [SECURITY.md](SECURITY.md) for a suspected
vulnerability and [the code of conduct](code-of-conduct.md) for a conduct
incident.

Questions about Services of type `LoadBalancer`, OCCM, Cinder CSI, or Manila
CSI belong to the
[cloud-provider-openstack](https://github.com/kubernetes/cloud-provider-openstack)
project. General Gateway API questions may be better suited to the
[Gateway API project](https://gateway-api.sigs.k8s.io/). This controller does
not own those projects and cannot set their support policy.

## What maintainers can investigate

Maintainers can investigate behavior in the documented controller scope:

- GatewayClass, Gateway, and HTTPRoute reconciliation owned by this controller
- controller-created Amphora, Neutron Floating IP, and route resources
- finalization and the read-only ownership audit
- the constrained NodePort backend path documented in the README

An issue may need help from the OpenStack operator when it depends on routing,
security groups, Octavia service health, quota, or cloud policy. The controller
does not have authority to repair infrastructure it does not own.

## Compatibility and release support

Compatibility is tied to a controller revision and a recorded environment. The
[compatibility evidence](docs/providers/compatibility.md) page explains the
required report. Until a release and environment appear there as supported,
successful use is experimental evidence only.

Pre-alpha releases may change flags, annotations, and experimental report
formats. Ownership metadata and cleanup compatibility receive special care
because a change there can strand or delete resources. Any breaking change must
be called out in release notes with an upgrade and cleanup path.
