# Test and compatibility reports

This directory holds durable reports for a particular controller revision and test environment.
A report is evidence for the scope it names.
It is not a general statement that the controller works on every OpenStack cloud.

Use these templates:

- [OpenStack controller E2E](openstack-e2e-template.md)
- [Gateway API conformance gap](conformance-gap-template.md)

Follow the [OpenStack E2E test guide](../testing-openstack-e2e.md) before using the controller report template.
The guide defines the dedicated and shared project modes, artifact pinning, result meanings, and cleanup order expected by the report.
Record the project mode, but do not copy expected project, subnet, or network IDs into a report.
For every mode, also omit ConfigMap and Secret UIDs, resourceVersions, and Pod template source annotation values.

Put completed reports in a directory named for the controller release or short commit, then use a short environment or report name.
Keep raw logs outside Git when they contain private identifiers.
Link only redacted artifacts that can be retained for as long as the report is used.

For a run in a shared project, keep the scoped ownership audit separate from the independent project-wide inventory.
The audit can return to an empty run scope while unrelated users continue to change the project.
Record who reviewed and attributed the before and after difference instead of requiring the whole project inventory to be identical.

Do not edit an old report to describe a newer release.
Add another report and link it from the compatibility matrix.

Templates and empty result tables are not evidence.
Keep a scenario as `Not run` until the named assertion has been exercised in the environment recorded by that report.
