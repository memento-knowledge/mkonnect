# Releasing mkonnect

Stable releases use annotated Git tags in the form `YYYYMMDD.n`. The first release on a calendar date is `.0`; every later release that day increases `n` by one with no gaps. Pushing a valid tag runs the Release workflow, which publishes calendar-tagged container images and a SemVer Helm chart, then creates a formal GitHub Release with the chart package, its checksum, and `release-metadata.json`.

Two container images are released from the same tag: the API connector (`Dockerfile`, repository `memento-connector`) and the git-sync connector (`Dockerfile.gitsync`, repository `memento-gitsync`). The connector image, the chart, and the GitHub Release never depend on the git-sync image; see [Non-blocking git-sync image](#non-blocking-git-sync-image).

## One-time AWS and GitHub setup

Create these public ECR repositories in `us-east-1` before the first release:

- `memento-connector` for the API connector container image
- `memento-gitsync` for the git-sync connector container image
- `mkonnect` for the Helm OCI chart

Without `memento-gitsync` the git-sync job fails, which is tolerated (see below) but means no git-sync image is published.

Amazon ECR Public does not offer a repository tag-immutability setting. The workflow protects release tags by reconciling existing artifacts only when their version, revision label, and digest agree. Do not manually overwrite a published release tag.

Set the public registry alias as a GitHub Actions repository variable:

```bash
gh variable set ECR_PUBLIC_ALIAS --repo memento-knowledge/mkonnect --body h2a8k0r3
```

Before trusting a release-tag OIDC subject, create a GitHub repository ruleset for `refs/tags/20*`. Restrict tag creation, updates, and deletion to the release maintainers, with no broad bypass. A user who can create a matching tag can otherwise start the release workflow and obtain its AWS identity. The workflow validates the exact `YYYYMMDD.n` form and the daily sequence.

Keep `AWS_ROLE_ARN` as a GitHub Actions secret. After the ruleset is active, the role's GitHub OIDC trust policy must accept `sts.amazonaws.com` as its audience and include this tag subject alongside any existing trusted subjects:

```json
{
  "StringLike": {
    "token.actions.githubusercontent.com:sub": "repo:memento-knowledge/mkonnect:ref:refs/tags/20*"
  },
  "StringEquals": {
    "token.actions.githubusercontent.com:aud": "sts.amazonaws.com"
  }
}
```

If the organization uses GitHub's repository-ID OIDC subject customization, inspect the repository ID with the following command and configure the trust policy for the exact customized `sub` format instead of adding the legacy ref subject:

```bash
gh api repos/memento-knowledge/mkonnect --jq .id
```

Grant the role `ecr-public:GetAuthorizationToken` and `sts:GetServiceBearerToken` with `Resource: "*"`. Restrict these ECR Public actions to the ARNs of only the three repositories above (`memento-connector`, `memento-gitsync`, and `mkonnect`): `ecr-public:BatchCheckLayerAvailability`, `ecr-public:CompleteLayerUpload`, `ecr-public:DescribeImages`, `ecr-public:InitiateLayerUpload`, `ecr-public:PutImage`, and `ecr-public:UploadLayerPart`.

The workflows use Helm `v3.18.6` for chart linting, packaging, and publication. Update that version only through a reviewed change that validates the new package format and OCI behavior.

## Non-blocking git-sync image

The Release workflow builds and publishes the git-sync image in its own job, `publish-gitsync`. The job is marked `continue-on-error` and has a 30-minute timeout. The `publish` job (connector image, chart, and GitHub Release) waits for the git-sync job to finish, so a hung git-sync job delays it by at most that 30-minute timeout. It then runs whether the git-sync job succeeded or failed; it does not run if the workflow was cancelled or verification failed.

Whether the git-sync image counts as published is decided by the digest the git-sync job exports, not by the job's result (with job-level `continue-on-error` the result can read `success` for a failed job). The publish job accepts the image only when the exported image is the expected `memento-gitsync` repository and the digest is a well-formed `sha256:` digest. Otherwise it records the image as not published.

- The guarantee is that no git-sync tag is pushed for a build that failed: the reconcile script pushes tags only after a successful build. The workflow shows a warning annotation instead of a failed run.
- Two edges remain, and neither affects the connector release. The release-ID tag is pushed first, so if the `sha-` tag push fails after it, the release-ID tag stays in the registry. While the GitHub Release is still a draft, a re-run completes the partial git-sync release from that tag. After the release is published, do not re-run the workflow: the stray release-ID tag stays, and the next release publishes git-sync again. If both pushes succeed but the job then fails (for example the final digest lookup fails or the timeout hits), the metadata records the git-sync image as not published while a tag exists. Installers key off `release-metadata.json`, so the stray tag is unused.
- The connector image, the Helm chart, and the GitHub Release are published as usual. The release notes state that the git-sync image was not published, and the git-sync fields in `release-metadata.json` are `null`.
- Both images use the same reconcile rules: an existing tag is accepted only when its version and revision labels match the release and its digest agrees with the other tag. The logic lives in `scripts/reconcile-image-tags.sh` and is tested by `scripts/reconcile-image-tags_test.sh`.
- A published GitHub Release cannot be re-run. A release published without a git-sync image stays that way; the next release publishes it again. Do not create git-sync tags by hand.
- Re-running is only for a release that is still a draft. Use "Re-run failed jobs" by default. "Re-run all jobs" is also accepted, but it re-runs the git-sync job too: if that job hits a transient error on the re-run, the regenerated metadata has `null` git-sync fields, and on a draft that replaces metadata which had recorded a published git-sync image (the image tags stay in the registry). See [Release metadata](#release-metadata).
- Do not publish the draft GitHub Release by hand while a workflow run is in progress: the workflow checks that the release is still a draft before it changes anything, and a manual publish between that check and its next step is not detected.

## Image signatures and provenance

Each release's connector and git-sync images are signed keylessly with [Sigstore cosign](https://docs.sigstore.dev/cosign/signing/overview/), and a SLSA build provenance attestation for each is stored in GitHub's attestation store. The images, release tags and digests do not change: the signature is attached to the digest the release already records in `release-metadata.json`, and the registry gains a signature artifact alongside the image (as an OCI referrer or under a `sha256-<digest>` tag).

Signing is best effort, like the git-sync image: the `sign-images` job runs after the GitHub Release is published, is marked `continue-on-error`, and signs the connector and git-sync images in separate steps. A signing failure shows as a warning annotation, never blocks or undoes a release, and a git-sync image that was not published is simply not signed. An image from a release whose signing step failed has no signature; check before relying on one.

The signing identity is this repository's release workflow at a release tag: the certificate's subject is `https://github.com/memento-knowledge/mkonnect/.github/workflows/release.yml@refs/tags/<release-id>` and its issuer is `https://token.actions.githubusercontent.com`. Keyless signatures are recorded in the public Sigstore transparency log (Rekor), which makes the repository, workflow and tag visible there.

Verify an image by digest (take the digest and release ID from `release-metadata.json`, never from a tag):

```bash
digest=$(jq -r .git_sync_image_digest release-metadata.json)
release_id=$(jq -r .release_id release-metadata.json)
cosign verify "public.ecr.aws/h2a8k0r3/memento-gitsync@$digest" \
  --certificate-identity-regexp '^https://github\.com/memento-knowledge/mkonnect/\.github/workflows/release\.yml@refs/tags/[0-9]{8}\.[0-9]+$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-github-workflow-repository memento-knowledge/mkonnect \
  --certificate-github-workflow-trigger push
```

Use `memento-connector` for the API connector image. Verify its provenance with the GitHub CLI (an alternative that does not depend on the registry):

```bash
gh attestation verify "oci://public.ecr.aws/h2a8k0r3/memento-gitsync@$digest" \
  --repo memento-knowledge/mkonnect \
  --signer-workflow memento-knowledge/mkonnect/.github/workflows/release.yml \
  --source-ref "refs/tags/$release_id"
```

A release published without signatures stays that way, as with the git-sync image: the next release is signed. Re-running a published release's workflow is not supported (see above). `scripts/sign-image.sh` is idempotent, though: it first checks whether a signature by the expected identity already verifies and signs only if not, so a future signing-only workflow can call it safely.

## Release metadata

Every GitHub Release has a `release-metadata.json` asset describing what was published. The file is UTF-8 JSON, pretty-printed with one key per line, a trailing newline, and exactly these keys in this order:

| Key | Meaning |
| --- | --- |
| `schema_version` | The number `1`. Changes only when the layout changes incompatibly. |
| `release_id` | The calendar release ID (`YYYYMMDD.n`), which is also the Git tag and the image tag. |
| `chart_version` | The SemVer version of the Helm chart published with this release. |
| `git_sync_release_id` | The image tag of the git-sync image (equal to `release_id`), or `null` when no git-sync image was published. |
| `connector_image_digest` | `sha256:` digest of the API connector image. Always present. |
| `git_sync_image_digest` | `sha256:` digest of the git-sync image, or `null` when no git-sync image was published. |

`null` means the git-sync image was not published for that release (its build or push failed). The two git-sync fields are always either both `null` or both set; every other field is always set. Consumers must treat `null` as "no git-sync image for this release" and not as an error.

`scripts/release-metadata-json.sh` is the only script that knows this layout, and `scripts/validate-release-metadata.sh` checks files against it. The workflow validates the generated file before uploading it and validates the uploaded asset again before publishing the release. The verify job also renders and validates the file with the real release ID and chart version (and placeholder digests) before anything is pushed, so a chart version the validator rejects fails the run early.

If a release already has `release-metadata.json`, the workflow regenerates it and compares (`scripts/reconcile-release-metadata.sh`):

- Identical: the asset is left alone.
- The release is still a draft and the git-sync fields change between `null` and a digest (both fields change together, in either direction): the asset is replaced. A draft has no consumers, and this keeps a re-run after a failed or recovered git-sync job from blocking the release.
- Anything else fails and nothing is changed: `release_id`, `chart_version`, or `connector_image_digest` differs; the git-sync digest changes from one value to a different one (a release tag would have changed, which never happens); or the release is already published. A published release's asset is never replaced.

Re-running a workflow for an unpublished release is designed to be safe. The verify job uploads its artifact with `overwrite: true`, so a new attempt of the same run can upload it again. The chart package is compared by content against the asset already on the draft release (see `scripts/release-asset-bundle_test.sh`), so a repackaged chart does not conflict. Use "Re-run failed jobs" by default. After the release is published, do not re-run the workflow.

To check a published release:

```bash
mkdir mkonnect-metadata-check
gh release download "$release_id" --repo memento-knowledge/mkonnect \
  --pattern release-metadata.json --dir mkonnect-metadata-check
bash scripts/validate-release-metadata.sh mkonnect-metadata-check/release-metadata.json
jq . mkonnect-metadata-check/release-metadata.json
```

## Prepare a release

Open and merge a release-preparation PR that updates both fields in `charts/mkonnect/Chart.yaml`. `version` is the next Helm-compatible SemVer chart version; `appVersion` is the calendar release ID that will tag the container image:

```yaml
version: X.Y.Z
appVersion: "YYYYMMDD.n"
```

Wait for the `main` CI run to succeed. Then tag that verified `main` commit:

```bash
release_id=20260919.0
git checkout main
git pull --ff-only origin main
git tag -a "$release_id" -m "mkonnect $release_id"
git push origin "$release_id"
```

The workflow rejects lightweight tags, invalid calendar dates and indexes, missing daily indexes, metadata mismatches, and tags that do not point to a `main` ancestor.

## Verify a release

Use the GitHub CLI to find and watch the Release workflow:

```bash
gh run list --repo memento-knowledge/mkonnect --workflow Release --limit 1
gh run watch <run-id> --repo memento-knowledge/mkonnect
gh release view "$release_id" --repo memento-knowledge/mkonnect
```

Verify the container image and its source labels:

```bash
docker pull "public.ecr.aws/h2a8k0r3/memento-connector:$release_id"
docker inspect "public.ecr.aws/h2a8k0r3/memento-connector:$release_id" \
  --format '{{ index .Config.Labels "org.opencontainers.image.version" }} {{ index .Config.Labels "org.opencontainers.image.revision" }}'
```

Verify the git-sync image when the release notes list one (the git-sync fields in `release-metadata.json` are not `null`). Its version and revision labels match the release, its digest matches the one in the metadata, and its `git` is 2.31 or newer:

```bash
git_sync_image="public.ecr.aws/h2a8k0r3/memento-gitsync"
docker pull "$git_sync_image:$release_id"
docker inspect "$git_sync_image:$release_id" \
  --format '{{ index .Config.Labels "org.opencontainers.image.version" }} {{ index .Config.Labels "org.opencontainers.image.revision" }}'
docker image inspect "$git_sync_image:$release_id" --format '{{ index .RepoDigests 0 }}'
docker run --rm "$git_sync_image:$release_id" --health
docker run --rm --entrypoint git "$git_sync_image:$release_id" --version
```

The digest in `RepoDigests` must equal `git_sync_image_digest` in `release-metadata.json`. The commit tag `sha-<release commit>` resolves to the same digest.

Verify the chart and the downloadable release asset:

```bash
chart_version=X.Y.Z
helm show chart oci://public.ecr.aws/h2a8k0r3/mkonnect --version "$chart_version"
mkdir mkonnect-release-check
gh release download "$release_id" --repo memento-knowledge/mkonnect \
  --pattern "mkonnect-$chart_version.tgz" \
  --pattern "mkonnect-$chart_version.tgz.sha256" \
  --dir mkonnect-release-check
(cd mkonnect-release-check && sha256sum -c "mkonnect-$chart_version.tgz.sha256")
```

## Update the git-sync base image

`Dockerfile.gitsync` pins its Alpine base layer by digest, so an upstream tag move cannot change the base of a release. The packages added on top (`git`, `ca-certificates`) are resolved from Alpine's package index at build time, so the image as a whole is not bit-for-bit reproducible, and a rebuild also picks up their patched versions; the build fails if `git` is older than 2.31. The consequence of the pin is that updates to the base layer itself do not arrive on their own. Maintainers must refresh the pin on a regular schedule (for example monthly) and promptly after a security advisory that affects Alpine, `musl`, `git`, `curl`, or OpenSSL packages.

1. Find the current stable Alpine release and the digest of its multi-platform index:

   ```bash
   docker buildx imagetools inspect alpine:latest
   ```

   The `Digest:` line at the top is the index digest to pin. The `org.opencontainers.image.version` annotation shows the full release (for example `3.24.3`); use its `major.minor` as the tag. Confirm that `docker buildx imagetools inspect alpine:<major.minor>` reports the same digest.
2. In `Dockerfile.gitsync`, update the `FROM alpine:<major.minor>@sha256:<digest>` line and the tag mentioned in the comment above it, together.
3. Build the image and check the tools it ships:

   ```bash
   docker build -f Dockerfile.gitsync -t mkonnect-gitsync:base-check .
   docker run --rm --entrypoint git mkonnect-gitsync:base-check --version
   docker run --rm mkonnect-gitsync:base-check --health
   ```

   `git` must be 2.31 or newer.
4. Open a pull request. The `image-gitsync` CI job rebuilds the image and re-runs the label, `git` version, `/data` ownership, and hardened-runtime checks; it must pass before merging.

The change takes effect for the next release tag. Images that were already published are never rebuilt.

## Recover safely

The workflow can safely complete a partial release only when an existing artifact matches the release version, source revision, and digest. This covers both container images, the chart, and `release-metadata.json`. If it reports a conflict, do not overwrite, delete, or retag an artifact. Fix the cause in a PR, increment the patch version, and create a new annotated tag.
