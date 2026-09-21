# Releasing and publishing

Images are published to GitHub Container Registry by
`.github/workflows/publish.yml` using the repository's `GITHUB_TOKEN`
(`packages: write` only). No long-lived credentials are stored anywhere.

## What gets published

| Event | Tags pushed to `ghcr.io/bmanhuge/plex-4k-transcode-guard` |
| --- | --- |
| Merge/push to `main` | `latest`, `sha-<short sha>` |
| Tag `vX.Y.Z` | `vX.Y.Z`, `vX.Y`, `vX`, `sha-<short sha>` |

Every image is a multi-arch index (`linux/amd64`, `linux/arm64`) with
provenance and SBOM attestations disabled, because the LinuxServer.io
`docker-mods` loader only understands plain platform manifests and only
extracts the first layer. The workflow fails closed unless:

1. the `sha-<short>` tag can be pulled anonymously (the package must be
   public; see below),
2. both platform images have exactly one layer, and
3. the in-container end-to-end test passes with the real
   `lscr.io/linuxserver/plex:latest` image loading the published tag through
   the real loader.

## Cutting a release

```bash
git switch main && git pull
git tag -a v1.0.0 -m "v1.0.0"
git push origin v1.0.0
```

Wait for the **Publish** workflow to finish, then check the step summary
for the digest. Create a GitHub Release from the tag with the changelog
entry (`CHANGELOG.md`).

## Package visibility (first publish only)

LinuxServer's loader downloads mods anonymously. If the very first publish
fails at "Verify anonymous pull", open
<https://github.com/bmanhuge?tab=packages>, select
`plex-4k-transcode-guard`, **Package settings → Change visibility →
Public**, then re-run the workflow. Packages created by a workflow in a
public repository normally inherit public visibility, so this is usually a
no-op.

## Pinning on the fleet

`:latest` moves with every merge to `main`; containers pick the new layer up
on their next start (the loader compares digests at every boot). For a
change-controlled fleet, reference an immutable tag instead:

```
DOCKER_MODS=ghcr.io/bmanhuge/plex-4k-transcode-guard:v1.0.0
```

## Local checks before tagging

```bash
make lint test race          # gofmt, vet, staticcheck, unit tests
make image verify-image      # single-layer image, --version runs
MOD_SOURCE=sideload:$PWD/modroot ./scripts/e2e-sideload.sh   # see README "Validation"
```
