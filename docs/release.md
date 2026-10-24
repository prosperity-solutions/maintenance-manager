# Releases

**Merge a `feat:` or `fix:` PR and a release ships itself.** No tags by hand, no
secrets, no release branch. The pipeline lives in the same workflow as CI
(`.github/workflows/ci.yml`), so nothing is published before CI is green.

## What triggers one

A push to `main` (every squash merge) runs CI, then:

```
all-green ─▶ publish ─▶ attest ─▶ release ─▶ verify
```

`publish` decides whether there is anything to release: the commit subjects
since the last published release must include a `feat` or `fix`, or carry `!`.
`docs:`, `chore:`, `ci:` and friends release nothing, and the run is still
green. The number comes from `svu next --v0`: the first release is `v0.1.0`,
`feat` bumps the minor, `fix` the patch, and `--v0` keeps breaking changes
below 1.0. Chart version = app version.

## What each job does

| job | does |
|---|---|
| `publish` | Fails if `vX.Y.Z` is already a published release. Bundles dependency licences into the image (`/var/run/ko/licenses/`). Builds the image with ko for linux/amd64 and linux/arm64, with ko's SPDX SBOM attached, and pushes `ghcr.io/prosperity-solutions/maintenance-manager:vX.Y.Z`. Pins that digest into the chart's `values.yaml`, packages the chart and pushes it to `oci://ghcr.io/prosperity-solutions/charts`. |
| `attest` | Signs SLSA build provenance for the image and the chart digests into GitHub's attestation store. No checkout, no shell. |
| `release` | Pulls the chart back by digest, writes `SHA256SUMS` (chart `.tgz` and the JS client `client/maintenance-manager.js`) and `THIRD_PARTY_LICENSES.csv`, creates a **draft** release, uploads and checks every asset, then publishes it. Publishing creates the tag and locks the release (immutable releases). |
| `verify` | `gh attestation verify` for both digests, pinned to `ci.yml` on `refs/heads/main`; the image has both platforms; the pulled chart renders. |

Digests and the version move between jobs only as job outputs. No artifacts, no
caches in release jobs, no secrets: `GITHUB_TOKEN` and GitHub OIDC only.

## The source of truth

**`vX.Y.Z` exists once its GitHub release is published, not before.** Until
then, registry tags for an unreleased version may be overwritten, which is how
a failed run recovers.

## When a release fails

- **Re-run the failed jobs** of the same commit: `gh run rerun --failed <run-id>`.
- **Failed before the release was published:** no tag exists, so the next
  releasable merge retries the same version and overwrites the registry tags.
- **A `.github/workflows/` change merged after the failing commit:** the
  release can no longer be created on the old commit (`GITHUB_TOKEN` cannot
  write workflow files and GitHub answers 404). Don't re-run; let the next
  releasable merge carry the version.
- **A bad release:** roll forward. Tags are never moved or deleted; merge a
  `fix:` and ship the next version.

## Verify and install

See [security.md](security.md#verify-before-you-install).
