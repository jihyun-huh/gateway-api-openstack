# Releasing gateway-api-openstack

This document describes the maintainer workflow for preparing binary release
artifacts. The project is pre-alpha. A draft or published GitHub release does
not by itself establish production readiness, OpenStack compatibility, or
Gateway API conformance.

## Current release scope

GoReleaser v2.17.1 builds release bundles for these Linux architectures:

- `linux/amd64`
- `linux/arm64`

Each bundle contains `openstack-gateway-controller`,
`openstack-gateway-audit`, `octavia-capability-probe`, `LICENSE`, `README.md`,
and `SECURITY.md`. A release also contains a source archive and
`checksums.txt` with SHA-256 checksums.

Controller images, Helm charts, signatures, provenance, and SBOMs remain
roadmap work. They require an agreed registry, signing identity, and promotion
policy. Until then, do not describe the binary bundle as a supported controller
image or claim those software supply chain guarantees.

## Validate locally

Use the Go version declared in `go.mod` and GoReleaser v2.17.1. Start from a
clean checkout with the complete Git history and tags, then run:

```sh
make verify
make build
make release-check
make release-snapshot
```

The snapshot command writes artifacts to `dist/` and never uploads them. Its
version is the next patch version followed by `-devel.` and an abbreviated
commit SHA, so local artifacts cannot be confused with a tagged release.

Inspect the result before proposing a release:

```sh
find dist -maxdepth 1 -type f -print
tar -tzf dist/gateway-api-openstack_v*_linux_amd64.tar.gz
(cd dist && shasum -a 256 -c checksums.txt)
```

Each architecture archive must contain all three executables and the three
project documents. The source archive and every binary archive must be covered
by `checksums.txt`.

## Prepare a release

1. Open a **New release** issue and use it as the review record. Link to CI
   results, compatibility reports, tests run in OpenStack, and conformance
   results instead of making general support claims.
2. Select a SemVer version with a `v` prefix. Release tags must point to a
   reviewed commit reachable from `main`.
3. Confirm the exact target commit is green in CI and repeat the local
   validation from a clean checkout.
4. Review README, the getting started guide, ROADMAP, `SUPPORT.md`,
   `SECURITY.md`, governance, and Amphora compatibility documentation. State
   missing evidence explicitly.

## Create the draft release

Create an annotated or signed tag. The workflow rejects lightweight tags,
non-SemVer tags, and tags whose commit is not reachable from `origin/main`.

```sh
git switch main
git pull --ff-only
version=v0.1.0
git tag -a "${version}" -m "Release ${version}"
git push origin "${version}"
```

Pushing the tag runs `.github/workflows/release.yml`. The workflow checks
module files and formatting, then runs `go vet` and the unit tests before
GoReleaser uploads the archives and checksums to a **draft** GitHub release. It
never publishes the release automatically. Do not create the draft in advance.
Reuse an existing draft only when retrying one that this automation created.

The workflow does not read the Changelog section from the release issue. Copy
the reviewed text into the draft, preserve the pre-alpha warning, and edit the
release notes to match the recorded evidence.

If the workflow fails before the draft is complete, fix the underlying cause
on `main` and choose a new version. Never move or reuse a tag that has been
pushed. If a retry encounters partial assets, delete only the unpublished draft
release, keep the tag, and rerun the failed workflow.

## Review and publish

Before publishing the draft:

1. Compare the tag's commit with the approved release issue target.
2. Download the assets and verify `checksums.txt` independently.
3. Extract the archive for each supported architecture and verify that all
   three binaries and the project documents are present.
4. Review the generated changelog, pre-alpha warning, compatibility evidence
   and test results for the release, upgrade actions, and known limitations.
5. Confirm no artifact or release note contains credentials, private cloud
   identifiers, or claims beyond the recorded evidence.
6. Publish the draft manually and link it from the release issue.

Published tags and artifacts are immutable project history. Correct a broken
release with a new patch version. Do not replace its tag or assets.
