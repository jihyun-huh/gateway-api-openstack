# OpenStack E2E validation summary

Fill in the relevant fields in the PR testing section.
Follow the [publication policy](README.md) for local artifacts and formal release reports.

## PR summary

- Tested revision: `<PR head or source revision, including any local changes>`.
- Command: `make test-e2e E2E_CONFIG=<private-config>`.
- Environment: `<Amphora, dedicated or shared project, Kubernetes and Gateway API versions, NodePort policy, VIP or Floating IP>`.
- Result: `<checks or behavior exercised and observed outcome>`.
- Cleanup: `<finalization, scoped audit, and independent inventory outcome, including limitations>`.
- Not run or unresolved: `<relevant coverage gaps, failures, or unexplained observations>`.
- Evidence: `<public CI run link, or local manual run with detailed evidence retained privately>`.

Include the date when it distinguishes multiple manual runs.
Add CNI, proxy mode, OpenStack versions, or topology when needed to explain a result.
The [test guide](../testing-openstack-e2e.md#audit-and-evidence-limits) defines result meanings and audit limits.
