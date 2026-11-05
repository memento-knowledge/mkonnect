#!/usr/bin/env bash
# Sign one container image keylessly with Sigstore cosign and check the signature verifies against
# this repository's release workflow identity.
#
# The image is addressed by digest only, never by tag: a tag can move, a digest cannot. The signature
# is attached to the existing digest, so nothing about the image or its tags changes.
#
# The step is idempotent: if a signature by the expected identity already verifies, nothing new is
# signed (cosign sign would otherwise add another signature on every re-run).
#
# Usage: sign-image.sh --image <registry/path> --digest sha256:<hex> --repository <owner/repo>
#
# Environment: none required. COSIGN is the cosign binary (default: cosign on PATH).
set -euo pipefail

usage() {
  echo "usage: $0 --image <registry/path> --digest sha256:<64 hex> --repository <owner/repo>" >&2
  exit 2
}

image=
digest=
repository=
while (($# > 0)); do
  case $1 in
    --image) image=${2-}; shift 2 ;;
    --digest) digest=${2-}; shift 2 ;;
    --repository) repository=${2-}; shift 2 ;;
    *) usage ;;
  esac
done
[[ -n $image && -n $digest && -n $repository ]] || usage

[[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || {
  printf 'refusing to sign: %s is not a sha256 digest\n' "$digest" >&2
  exit 2
}
[[ $image =~ ^[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9._-]+)+$ ]] || {
  printf 'refusing to sign: %s is not a registry path without a tag\n' "$image" >&2
  exit 2
}
[[ $repository =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]] || {
  printf 'refusing to sign: %s is not an owner/repo name\n' "$repository" >&2
  exit 2
}

cosign=${COSIGN:-cosign}
ref="$image@$digest"
issuer=https://token.actions.githubusercontent.com
# The certificate's identity is the workflow that signed, at the release tag (YYYYMMDD.n) it ran for.
repo_regex=${repository//./\\.}
identity_regexp="^https://github\\.com/${repo_regex}/\\.github/workflows/release\\.yml@refs/tags/[0-9]{8}\\.[0-9]+\$"

verify() {
  "$cosign" verify "$ref" \
    --certificate-identity-regexp "$identity_regexp" \
    --certificate-oidc-issuer "$issuer" \
    --certificate-github-workflow-repository "$repository" \
    --certificate-github-workflow-trigger push >/dev/null
}

precheck=$(mktemp)
trap 'rm -f "$precheck"' EXIT
if verify 2>"$precheck"; then
  printf 'already signed by this workflow identity: %s\n' "$ref"
  exit 0
fi
# Not (verifiably) signed yet. Show why, so a transient failure of the check is not mistaken for
# an unsigned image in the log.
printf 'no valid signature found before signing (this is expected for a new release); cosign said:\n' >&2
sed 's/^/  /' "$precheck" >&2

printf 'signing %s\n' "$ref"
"$cosign" sign --yes "$ref"
verify
printf 'signed and verified: %s\n' "$ref"
