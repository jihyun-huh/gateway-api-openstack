# Gateway API conformance gap report: controller revision

Status: Draft

Date: YYYY-MM-DD

Gateway API version:

Controller release or commit:

Environment report:

## Invocation

Record the exact upstream suite revision, profile, supported feature flags,
command, and unmodified result artifact. This is a local gap analysis unless an
accepted upstream report says otherwise.

## Summary

| Result | Count |
| --- | ---: |
| Passed | 0 |
| Failed | 0 |
| Skipped | 0 |

## Gap classification

Classify every failed or skipped Core test.

| Test | Result | Classification | Reason | Follow-up |
| --- | --- | --- | --- | --- |
| Example | Failed | Controller defect | Replace with the actual finding | Issue link |

Use one of these classifications:

- controller defect
- compiler work
- backend mode or topology requirement
- Octavia API limitation
- planned Gateway API extension
- intentional non-goal
- environment or harness failure

## Feature claims

List the `GatewayClass.status.supportedFeatures` observed during the run. Explain
any difference between those runtime claims and the test selection. Do not add
a feature merely to make a test run, or remove one merely to hide a failure.

## Conclusion

State whether the result is eligible for upstream submission under the current
Gateway API rules. List the work needed for the next report and the Core
semantics that may be structurally unrepresentable on Amphora.
