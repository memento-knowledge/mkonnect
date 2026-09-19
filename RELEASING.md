# Releasing mkonnect

Stable releases use annotated Git tags in the form `vMAJOR.MINOR.PATCH`. Pushing a valid tag runs the Release workflow, which publishes a versioned container image and Helm chart, then creates a formal GitHub Release with the chart package and checksum.

## One-time AWS and GitHub setup

Create these public ECR repositories in `us-east-1` before the first release:

- `memento-connector` for the container image
- `mkonnect` for the Helm OCI chart

Amazon ECR Public does not offer a repository tag-immutability setting. The workflow protects release tags by reconciling existing artifacts only when their version, revision label, and digest agree. Do not manually overwrite a published release tag.

Set the public registry alias as a GitHub Actions repository variable:

```bash
gh variable set ECR_PUBLIC_ALIAS --repo memento-knowledge/mkonnect --body h2a8k0r3
```

Before trusting a release-tag OIDC subject, create a GitHub repository ruleset for `refs/tags/v*`. Restrict tag creation, updates, and deletion to the release maintainers, with no broad bypass. A user who can create a matching tag can otherwise start the release workflow and obtain its AWS identity.

Keep `AWS_ROLE_ARN` as a GitHub Actions secret. After the ruleset is active, the role's GitHub OIDC trust policy must accept `sts.amazonaws.com` as its audience and include this tag subject alongside any existing trusted subjects:

```json
{
  "StringLike": {
    "token.actions.githubusercontent.com:sub": "repo:memento-knowledge/mkonnect:ref:refs/tags/v*"
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

Grant the role `ecr-public:GetAuthorizationToken` and `sts:GetServiceBearerToken` with `Resource: "*"`. Restrict these ECR Public actions to the ARNs of only the two repositories above: `ecr-public:BatchCheckLayerAvailability`, `ecr-public:CompleteLayerUpload`, `ecr-public:DescribeImages`, `ecr-public:InitiateLayerUpload`, `ecr-public:PutImage`, and `ecr-public:UploadLayerPart`.

The workflows use Helm `v3.18.6` for chart linting, packaging, and publication. Update that version only through a reviewed change that validates the new package format and OCI behavior.

## Prepare a release

Open and merge a release-preparation PR that updates both fields in `charts/mkonnect/Chart.yaml` to the target version:

```yaml
version: X.Y.Z
appVersion: "X.Y.Z"
```

Wait for the `main` CI run to succeed. Then tag that verified `main` commit:

```bash
release_version=0.1.0
git checkout main
git pull --ff-only origin main
git tag -a "v$release_version" -m "mkonnect $release_version"
git push origin "v$release_version"
```

The workflow rejects lightweight tags, tags with invalid stable SemVer, metadata mismatches, and tags that do not point to a `main` ancestor.

## Verify a release

Use the GitHub CLI to find and watch the Release workflow:

```bash
gh run list --repo memento-knowledge/mkonnect --workflow Release --limit 1
gh run watch <run-id> --repo memento-knowledge/mkonnect
gh release view "v$release_version" --repo memento-knowledge/mkonnect
```

Verify the container image and its source labels:

```bash
docker pull "public.ecr.aws/h2a8k0r3/memento-connector:$release_version"
docker inspect "public.ecr.aws/h2a8k0r3/memento-connector:$release_version" \
  --format '{{ index .Config.Labels "org.opencontainers.image.version" }} {{ index .Config.Labels "org.opencontainers.image.revision" }}'
```

Verify the chart and the downloadable release asset:

```bash
helm show chart oci://public.ecr.aws/h2a8k0r3/mkonnect --version "$release_version"
mkdir mkonnect-release-check
gh release download "v$release_version" --repo memento-knowledge/mkonnect \
  --pattern "mkonnect-$release_version.tgz" \
  --pattern "mkonnect-$release_version.tgz.sha256" \
  --dir mkonnect-release-check
(cd mkonnect-release-check && sha256sum -c "mkonnect-$release_version.tgz.sha256")
```

## Recover safely

The workflow can safely complete a partial release only when an existing artifact matches the release version, source revision, and digest. If it reports a conflict, do not overwrite, delete, or retag an artifact. Fix the cause in a PR, increment the patch version, and create a new annotated tag.
