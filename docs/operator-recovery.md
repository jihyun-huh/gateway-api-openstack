# Recover from blocked finalization

This guide covers a Gateway or HTTPRoute that cannot finish cleanup. The
controller stops finalization when an OpenStack request fails or ownership
cannot be proved. That behavior protects resources owned by another controller
or cluster.

Keep the controller and its original OpenStack access available while you
investigate. Do not remove its finalizers, edit identity annotations, or delete
resources by a matching name. Those actions bypass the checks that make cleanup
safe.

## Check the current failure

Start with the Kubernetes object that is still deleting. Review its conditions,
Events, and controller logs. The public messages are deliberately brief and do
not include OpenStack response bodies.

```sh
kubectl -n <namespace> describe gateway <name>
kubectl -n <namespace> describe httproute <name>
kubectl -n openstack-gateway-system logs \
  deployment/openstack-gateway-controller --all-containers
```

Confirm these settings before changing anything:

- The controller name is the same value that created the resources.
- The cluster ID has not changed.
- The credential is scoped to the original OpenStack project.
- The configured region still selects the same Octavia and Neutron endpoints.
- The Gateway API v1.6.1 CRDs are installed as one Standard Channel bundle.
  A bundle mismatch blocks provisioning and repair, but it does not block
  finalization.
- Octavia and Neutron are reachable, the project has enough quota, and the
  controller credential can read and delete the resources it owns.

Authentication, authorization, quota, rate limiting, and service failures are
different conditions. Restore the original access or service health and let
the controller try again. A new credential is suitable only when it is scoped
to the same project and has the required permissions.

## Run the ownership audit

`openstack-gateway-audit` compares durable Gateway and HTTPRoute bindings with
resources found in the authenticated OpenStack project. It only lists and gets
resources. It does not patch Kubernetes, adopt OpenStack resources, delete
anything, or remove a finalizer.

Build the tool from the same reviewed source tree as the controller when a
release bundle is not available:

```sh
make build-audit VERSION="$(git describe --always --dirty)"
```

Use the controller name, cluster ID, cloud entry, region, and Octavia
microversion from the affected installation. The command follows the usual
kubeconfig loading rules. Use `--kubeconfig` and `--context` when the default
context is not the affected cluster.

```sh
umask 077
bin/openstack-gateway-audit \
  --controller-name=example.net/gateway-api-openstack \
  --cluster-id=<stable-cluster-id> \
  --kubeconfig=<path-to-kubeconfig> \
  --context=<context-name> \
  --clouds-yaml=<path-to-clouds.yaml> \
  --openstack-cloud=<cloud-name> \
  --openstack-region=<region> \
  --octavia-microversion=<microversion> \
  > ownership-audit.json
```

When `--clouds-yaml` is omitted, the command uses `OS_CLIENT_CONFIG_FILE` when
it is set. Otherwise it reads the standard OpenStack authentication environment
variables. `--openstack-cloud` defaults to `OS_CLOUD` and is required whenever
a clouds.yaml path is selected. `--openstack-region` defaults to
`OS_REGION_NAME`. The Kubernetes identity needs list access to Gateways and
HTTPRoutes. The OpenStack credential needs read access to the relevant Octavia
and Neutron APIs.

The command writes only one JSON document to standard output. Errors go to
standard error. It exits with status 0 after writing a report, even when the
report has findings. Pass `--fail-on-findings` to receive status 2 after a valid
report with findings. Do not use this status to trigger cleanup. The JSON
schema is experimental. Configuration, authentication, or read failures return
status 1 without a report.

The report contains OpenStack resource IDs and Kubernetes namespaces, names,
and UIDs for local investigation. It also contains the cluster ID and
controller name. Keep the file private and remove those identifiers before
sharing it outside the operations team.

## Read the report

The `assessment` field describes the Kubernetes evidence used for the
comparison:

- `complete` means every durable binding detected by the command was valid.
- `incomplete` means at least one object had missing or conflicting binding
  metadata. OpenStack orphan candidates cannot be evaluated safely in this
  state.

A complete assessment does not prove that every desired OpenStack resource
exists, that no resource leaked, or that deletion is safe. Kubernetes and
OpenStack are listed separately, so state may change between observations.
Let reconciliation settle and run the command again before drawing a
conclusion.

Each OpenStack resource has one disposition:

| Disposition | Operator meaning |
| --- | --- |
| `matched` | The resource identity and observed parent match a current binding. This says nothing about the completeness of the rest of the graph. |
| `orphanCandidate` | The resource has this controller's complete scope identity, but no current valid binding matches it. Investigate its history. This is not permission to delete it. |
| `staleUID` | Object names match a current binding, but an immutable Kubernetes UID differs. Check whether an object was deleted and recreated. |
| `ownershipConflict` | Identity, project, parent, or cardinality evidence conflicts. Stop manual mutation and resolve the source of the conflict. |
| `unresolved` | The available observations do not support a safe conclusion. Gather the missing parent or identity evidence. |

The `kubernetesIssues` list identifies objects whose stored binding could not
be used. Restore binding metadata only from an authoritative backup for the
same Kubernetes object UID. If installation configuration changed, restore the
exact controller name, cluster ID, region, and project scope used by that
installation. Never construct a binding from resource names or copy
annotations from another object.

## Let the controller resume

After correcting credentials, permissions, quota, service health, or the
original binding configuration, leave the object and its finalizers in place.
The controller will observe the graph again and continue cleanup in dependency
order. Run the audit again after the Kubernetes object finishes deletion or the
failure condition changes.

This project does not provide a supported manual cleanup command. If normal
reconciliation cannot be restored, preserve the report and controller logs and
open a focused recovery issue with identifiers removed. Any manual OpenStack
change needs its own review and ownership proof immediately before mutation; a
saved audit report is not sufficient.

## Discovery limits

The audit cannot find resources after their controller or cluster identity
tags have been removed. Members and L7 rules are found only through trusted
parents. A detached Floating IP that no longer matches a current binding cannot
be attributed from its description digest alone and is omitted. The audit also
does not check whether every Kubernetes binding has a complete desired graph.

Because of these limits, never use the report for broad project cleanup. Keep
unrelated OpenStack resources outside the scope of any recovery action.
