# Test and compatibility reports

This directory holds durable reports for a particular controller revision and
test environment. A report is evidence for the scope it names. It is not a
general statement that the controller works on every OpenStack cloud.

Use these templates:

- [OpenStack controller E2E](openstack-e2e-template.md)
- [Gateway API conformance gap](conformance-gap-template.md)

Put completed reports in a directory named for the controller release or short
commit, then use a short environment or report name. Keep raw logs outside Git
when they contain private identifiers. Link only redacted artifacts that can be
retained for as long as the report is used.

Do not edit an old report to describe a newer release. Add another report and
link it from the compatibility matrix.
