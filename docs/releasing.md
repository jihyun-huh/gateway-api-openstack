# Releasing gateway-api-openstack

This document describes the maintainer workflow for preparing binary release
artifacts. The project is pre-alpha. A draft or published GitHub release does
not by itself establish production readiness, OpenStack compatibility, or
Gateway API conformance.

## Current release scope

GoReleaser v2.17.1 builds a deterministic release bundle for each supported
Linux architecture:

- `linux/amd64`;
- `linux/arm64`.

Each bundle contains `openstack-gateway-controller`,
`octavia-capability-probe`, `LICENSE`, `README.md`, and `SECURITY.md`. A release
also contains a source archive and `checksums.txt` with SHA-256 checksums.

The current automation does not publish a controller image or Helm chart and
does not generate signatures, provenance, or SBOMs. Those require an accepted
registry, identity, promotion, and signing policy and remain part of the
release-hardening roadmap. Do not describe the binary bundle as a supported
controller image or claim those supply-chain guarantees are present.

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
version is the next patch version followed by `-devel.<short-commit>`, so local
artifacts cannot be confused with a tagged release.

Inspect the result before proposing a release:

```sh
find dist -maxdepth 1 -type f -print
tar -tzf dist/gateway-api-openstack_v*_linux_amd64.tar.gz
(cd dist && shasum -a 256 -c checksums.txt)
```

Both binary archives must contain the two expected executables and the three
project documents. The source archive and every binary archive must be covered
by `checksums.txt`.

## Prepare a release

1. Open a **New release** issue and use it as the review record. Link concrete
   CI, compatibility, real-cloud, and conformance evidence rather than making
   general support claims.
2. Select a SemVer version with a `v` prefix. Release tags must point to a
   reviewed commit contained in `main`.
3. Confirm the exact target commit is green in CI and repeat the local
   validation from a clean checkout.
4. Review README, getting-started guidance, ROADMAP, `SECURITY.md`, and Amphora
   compatibility documentation. State missing evidence explicitly.

## Create the draft release

Create an annotated or signed tag. The workflow rejects lightweight tags,
non-SemVer tags, and tags whose commit is not contained in `origin/main`.

```sh
git switch main
git pull --ff-only
version=v0.1.0
git tag -a "${version}" -m "Release ${version}"
git push origin "${version}"
```

Pushing the tag runs `.github/workflows/release.yml`. The workflow repeats
module, formatting, vet, and unit-test verification before GoReleaser uploads
the archives and checksums to a **draft** GitHub release. It never publishes
the release automatically. Do not create the draft in advance. Reusing an
existing draft is intended only for retrying a draft that this automation
already created.

The workflow-generated changelog is only a starting point. It does not read
the Changelog markers in the release issue. Copy the reviewed Changelog block
from the issue into the draft, then preserve the pre-alpha warning and edit the
release notes to match the recorded evidence.

If the workflow fails before the draft is complete, fix the underlying cause
on `main` and choose a new version. Never move or reuse a tag that has been
pushed. If a retry encounters partial assets, delete only the unpublished draft
release, keep the tag, and re-run the failed workflow.

## Review and publish

Before publishing the draft:

1. Compare the tag's commit with the approved release issue target.
2. Download the assets and verify `checksums.txt` independently.
3. Extract both architectures and verify the expected binaries and project
   documents are present.
4. Review the generated changelog, pre-alpha warning, exact compatibility and
   test evidence, upgrade actions, and known limitations.
5. Confirm no artifact or release note contains credentials, private cloud
   identifiers, or claims beyond the recorded evidence.
6. Publish the draft manually and link it from the release issue.

Published tags and artifacts are immutable project history. Correct a broken
release with a new patch version; do not replace its tag or assets.
