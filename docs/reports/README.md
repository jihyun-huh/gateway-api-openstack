# Test evidence and publication

Commit reusable tests and instructions, summarize validation in the PR, and retain detailed output in local or CI artifacts.
Development runs do not need a committed report.
Release and conformance claims still require reviewed public evidence.

## What goes where

| Material | Default location | Public content |
| --- | --- | --- |
| Test code, regression cases, synthetic fixtures, runner examples, and execution instructions | Repository | Enough for contributors to run the same checks with their own environment |
| Validation for an ordinary change | PR testing section | Command, tested scope, results, cleanup, material limitations, and a CI link when available |
| Generated reports, logs, metrics, resource snapshots, and investigation notes | Ignored local directory or reviewed CI artifacts | Only the redacted excerpt needed to explain a failure |
| Internal cloud and cluster names, private endpoints and IDs, populated configuration, local image reference and digest | Private local run record | Omit unless a specific, authorized detail is necessary to understand a defect |
| Credentials, tokens, kubeconfig, certificates, and credential-bearing dumps | Protected files outside the report bundle | Never publish in Git, PR text, or CI artifacts |
| Release compatibility decisions and conformance submissions | Reviewed public report | Relevant versions, topology, public release artifact identity, results, limitations, and durable supporting links |

Pin test images by digest and retain those digests locally so the run can be reproduced.
Include a digest in the public summary when the image is available to reviewers and its identity matters to a release, compatibility claim, or defect.

## Minimum PR evidence

Use the [validation template](openstack-e2e-template.md) for the tested revision, command, relevant environment, results, cleanup, and remaining gaps.
Identify any local changes so the result is not attributed to an untested PR revision.
Summarize unresolved failures even if a later retry passed.
Link a public CI run when available, or state that the checks ran locally and the details are retained privately.
Private filesystem paths are not useful links in a public PR.

## Local and CI artifacts

The repository ignores `_artifacts/` and `test-results/`.
Use a separate directory per run and retain generated reports, exact artifact identity, and investigation history through review and any related investigation.
Keep credentials and sensitive raw output in protected storage outside the artifact bundle.
Git ignore rules do not restrict file access.

For shared projects, compare independent inventories from before and after the test and account for every change before claiming cleanup is complete.
The PR needs the outcome and any limitation, not the tenant inventory or other users' resource counts.

CI artifacts may be public and may expire.
Check the contents and access settings before uploading.
Preserve public release evidence for as long as the project makes the claim it supports.
[Prow documents automatic artifact upload and the risk of leaking secrets through public logs and artifacts](https://docs.prow.k8s.io/docs/components/pod-utilities/).

## Related guidance

The [Cluster API contribution guide](https://github.com/kubernetes-sigs/cluster-api/blob/main/CONTRIBUTING.md#triaging-pr-or-periodic-test-failures), [CAPO CI guide](https://cluster-api-openstack.sigs.k8s.io/development/ci#prow), and [cloud-provider-openstack CI runner](https://github.com/kubernetes/cloud-provider-openstack/blob/master/tests/ci-occm-e2e.sh) illustrate PR results backed by separate test artifacts.
Omitting internal cloud branding and local image digests is this project's publication policy, not a claim that upstream forbids those fields.
Use the [conformance gap template](conformance-gap-template.md) for separate conformance work and [release guidance](../releasing.md) for release evidence.
