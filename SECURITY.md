# Security policy

## Project status

The project is pre-alpha and does not yet have a supported production release.
This does not reduce the importance of reporting vulnerabilities responsibly.

## Reporting a vulnerability

Do not open a public GitHub issue.

Use GitHub's private vulnerability reporting feature for this repository. The
repository owner must enable that feature before publishing the first
controller release.

Include:

- affected version or commit
- impact and threat model
- reproduction details
- whether credentials, OpenStack project isolation, resource deletion, or traffic
  exposure is involved
- any suggested mitigation
- your preferred contact details and disclosure timeline

Never send real OpenStack credentials or customer data. Use redacted
configuration and a minimal reproduction.

## Response

Maintainers will acknowledge private reports as soon as practical, assess the
affected versions, and coordinate a fix and disclosure. The project will
publish formal response times before its first production release.

## Scope

Security concerns include:

- Keystone credentials and Kubernetes Secrets
- cross-namespace references
- OpenStack project and resource isolation
- resource identity, adoption, and deletion
- OpenStack security group management
- TLS material and Barbican
- controller RBAC
- release images and the software supply chain
