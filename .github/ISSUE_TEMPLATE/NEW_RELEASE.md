---
name: New release
about: Track and review a gateway-api-openstack release
title: "Release v0.x.0"
labels: ""
assignees: ""
---

## Release metadata

- Version: `v0.x.0`
- Target commit:
- Previous release:
- Release owner:
- Planned date:

## Release checklist

<!-- Do not remove checklist items. Mark an item only when evidence is linked. -->

- [ ] At least one maintainer approved this release proposal.
- [ ] The version follows SemVer and the target commit is on `main`.
- [ ] CI is green for the exact target commit.
- [ ] `make verify`, `make build`, `make release-check`, and
      `make release-snapshot` pass from a clean checkout.
- [ ] The snapshot contains Linux `amd64` and `arm64` archives with both
      binaries, `LICENSE`, `README.md`, and `SECURITY.md`.
- [ ] `checksums.txt` verifies every generated archive.
- [ ] README, getting-started guidance, ROADMAP, security policy, and provider
      compatibility evidence are current for this release.
- [ ] Exact tested Kubernetes, Gateway API, OpenStack, Octavia, and provider
      versions are recorded below. Missing real-cloud or conformance evidence
      is stated explicitly.
- [ ] Release notes describe breaking changes, known limitations, unsupported
      Gateway API behavior, and any operator action required for upgrades.
- [ ] Release notes and artifacts contain no credentials, private cloud
      identifiers, customer data, or unsupported compatibility claims.
- [ ] GitHub private vulnerability reporting is enabled before the first
      public release.
- [ ] An annotated or signed `vX.Y.Z` tag was created at the reviewed commit
      and pushed without moving or reusing an existing tag.
- [ ] The Release workflow succeeded and created a draft GitHub release.
- [ ] The draft contains two binary archives, one source archive, and
      `checksums.txt`, with names and versions matching the tag.
- [ ] The reviewed Changelog block below was copied into the draft release;
      workflow-generated notes were treated only as a starting point.
- [ ] The draft warning, generated changelog, compatibility evidence, and
      known limitations were reviewed and edited as needed.
- [ ] The draft was published manually only after all checks above passed.
- [ ] Published downloads and checksums were verified, and this issue was
      closed with a link to the release.

## Compatibility and test evidence

<!-- Include exact versions, topology, commands, and links to redacted results. -->

## Changelog

<!-- The release workflow does not read these markers. Copy the reviewed block
     into the draft release before publishing it. -->

<!-- release-changelog-start -->

```markdown
### Highlights

Describe the main changes.

### Breaking changes and operator actions

Describe required actions, or write "None".

### Known limitations

Describe unsupported behavior and missing evidence.
```

<!-- release-changelog-end -->
