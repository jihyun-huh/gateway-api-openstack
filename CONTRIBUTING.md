# Contributing to gateway-api-openstack

Thanks for taking the time to help. The project is still pre-alpha, so a good
contribution is often a test result, a small design question, or a clear bug
report rather than a large feature.

Please read the [code of conduct](code-of-conduct.md) before participating.
Project roles and the decision process are described in
[GOVERNANCE.md](GOVERNANCE.md).

## Before you start

Search the issue tracker before opening a new issue. Use the bug report for a
controller defect, the environment report for results from an Amphora cloud,
and the feature request for a new behavior.

Small fixes can go straight to a pull request. Open a design issue first when a
change affects any of these areas:

- cloud ownership or deletion authority
- immutable resource identity
- the writer for a Gateway graph
- the controller name, API domain, or a public API
- GatewayClass parameters or migration behavior
- credentials, member mode, or provider capabilities
- a shared field on an object owned by another controller
- a Gateway API conformance claim

These changes need an accepted architecture decision record (ADR) before the
implementation is merged. The [ADR process](docs/design/adr/README.md) explains
how to start one. An issue or an implementation detail is not an accepted
decision.

If you are looking for a first contribution, issues labeled `good first issue`
should be possible without access to an OpenStack cloud. Issues labeled
`help wanted` are expected to have enough context for somebody outside the
current maintainer group to pick them up. Ask on the issue if that is not true.

## Set up a development environment

Use the Go version in `go.mod`. Unit tests use fake Kubernetes clients or fake
OpenStack HTTP servers and do not need cloud credentials.

Run the normal checks with:

```sh
make verify
make test-race
```

Controller tests that need API server behavior use envtest:

```sh
make envtest-assets
make test-envtest
```

The first command downloads the pinned Kubernetes control plane binaries. The
envtest suite installs the Gateway API Standard Channel CRDs pinned by this
repository. It does not run Octavia, send traffic, or provide Gateway API
conformance evidence.

Run `make lint` when `golangci-lint` is installed. Use `make fmt` for Go source
formatting. Do not run `make manifests`, `make generate`, or Kubebuilder
scaffolding commands. This repository consumes the upstream Gateway API types
and maintains its Kustomize and RBAC files by hand.

Never put `clouds.yaml`, application credential secrets, tokens, certificates,
private addresses, project IDs, or customer information in an issue, test
fixture, log, or pull request.

## Make a change

Keep a pull request focused. A refactor should normally be separate from a
behavior change, especially in reconciliation, finalization, and ownership
code. Add the Apache 2.0 boilerplate from `hack/boilerplate` to new Go files.

Follow the dependency boundaries in the architecture document:

- `cmd` parses configuration and wires dependencies
- `internal/controller` owns shared Kubernetes-facing contracts
- `internal/controller/gatewayclass`, `internal/controller/gateway`, and
  `internal/controller/httproute` own their reconciler lifecycle and status
  behavior
- `internal/controller/graph` owns only process-local coordination by Gateway
  UID; it is not the proposed single graph writer
- `internal/cloud` defines provider-neutral values and interfaces
- `internal/cloud/openstack` is the command-facing OpenStack facade
- packages below `internal/cloud/openstack` own clients, service operations,
  existing provider graph operations, identity, error classification, and
  read-only inventory
- `internal/audit` builds provider-neutral audit reports

The [development layout guide](docs/development-layout.md) shows the current
tree and dependency direction. Do not import Gophercloud from a controller
package, import Kubernetes types from an OpenStack package, or import the root
OpenStack facade from one of its child packages.

When a reconciler starts reading or writing another Kubernetes resource,
update both its RBAC markers and `config/rbac/cluster_role.yaml`. Preserve
status fields owned by other controllers.

Every cloud lifecycle change should cover the relevant create, converged
no-op, restart, partial graph, external deletion, drift, ownership conflict,
and repeated deletion cases. Count mutating provider calls where a minimal diff
is part of the contract.

## Open a pull request

Fill in the pull request template and include:

- the user or operator problem
- the important design choices
- exact tests run
- OpenStack environment details when cloud behavior was tested
- API, ownership, compatibility, and conformance impact

All required CI checks must pass. A test in one OpenStack project is useful
evidence for that environment, but it is not a general compatibility claim.
Put reusable environment results in the compatibility report format described
in [docs/providers/compatibility.md](docs/providers/compatibility.md).

Reviewers check correctness and test coverage. Approvers check the wider API,
ownership, compatibility, and project direction. The root `OWNERS` file is the
current source for those roles. Until the project has more than one approver,
independent maintainer approval is not always possible; the limitation is
recorded openly rather than filled with inactive names.

## Documentation

Write documentation in U.S. English and use the Kubernetes documentation style:
direct sentences, sentence case headings, and standard Kubernetes and Gateway
API terminology. Describe current behavior separately from planned behavior.
Do not use “supported,” “conformant,” or “production-ready” unless the matching
evidence is published.

## License

By contributing, you agree that your contribution is licensed under the
[Apache License 2.0](LICENSE). The project does not currently run the CNCF CLA
bot. A future move into a community organization may require contributors to
complete that organization's contribution process.
