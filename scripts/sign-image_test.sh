#!/usr/bin/env bash

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
script="$root/scripts/sign-image.sh"
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

image=registry.example.invalid/alias/memento-example
digest=sha256:$(printf 'a%.0s' {1..64})
repository=example-org/example.repo

# A fake cosign that records its arguments. FAKE_SIGNED=1 makes `verify` succeed from the start;
# otherwise it succeeds only after a `sign`. FAKE_SIGN_FAIL / FAKE_VERIFY_FAIL force failures.
cat >"$tmpdir/cosign" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$FAKE_LOG"
case $1 in
  verify)
    [[ -z ${FAKE_VERIFY_FAIL-} ]] || exit 1
    [[ -n ${FAKE_SIGNED-} || -f $FAKE_STATE ]] || exit 1
    ;;
  sign)
    [[ -z ${FAKE_SIGN_FAIL-} ]] || exit 1
    : >"$FAKE_STATE"
    ;;
esac
STUB
chmod +x "$tmpdir/cosign"

run() {
  : >"$tmpdir/log"
  rm -f "$tmpdir/state"
  COSIGN="$tmpdir/cosign" FAKE_LOG="$tmpdir/log" FAKE_STATE="$tmpdir/state" "$@" 2>"$tmpdir/stderr"
}
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  sed 's/^/  log: /' "$tmpdir/log" >&2
  sed 's/^/  stderr: /' "$tmpdir/stderr" >&2
  exit 1
}
count() { grep -c -- "$1" "$tmpdir/log" || true; }

# Not yet signed: verify (fails), sign, verify (passes).
run bash "$script" --image "$image" --digest "$digest" --repository "$repository" >/dev/null ||
  fail "first signing should succeed"
[[ $(count '^sign ') == 1 ]] || fail "expected exactly one sign"
[[ $(count '^verify ') == 2 ]] || fail "expected a verify before and after signing"
grep -q -- "^sign --yes $image@$digest\$" "$tmpdir/log" || fail "must sign by digest, not by tag"
grep -q -- "--certificate-oidc-issuer https://token.actions.githubusercontent.com" "$tmpdir/log" ||
  fail "must pin the GitHub Actions issuer"
grep -q -F -- '--certificate-identity-regexp ^https://github\.com/example-org/example\.repo/\.github/workflows/release\.yml@refs/tags/[0-9]{8}\.[0-9]+$' "$tmpdir/log" ||
  fail "must pin the release workflow identity at a release tag, with dots escaped"

grep -q -- "--certificate-github-workflow-repository $repository --certificate-github-workflow-trigger push" "$tmpdir/log" ||
  fail "must pin the repository and the push trigger of the signing workflow"

# Already signed: nothing new is signed.
FAKE_SIGNED=1 run bash "$script" --image "$image" --digest "$digest" --repository "$repository" >/dev/null ||
  fail "an already signed image should succeed"
[[ $(count '^sign ') == 0 ]] || fail "an already signed image must not be signed again"

# A signing failure fails the script.
if FAKE_SIGN_FAIL=1 run bash "$script" --image "$image" --digest "$digest" --repository "$repository" >/dev/null; then
  fail "a failed sign must fail"
fi
# A signature that does not verify afterwards fails the script.
if FAKE_VERIFY_FAIL=1 run bash "$script" --image "$image" --digest "$digest" --repository "$repository" >/dev/null; then
  fail "a signature that does not verify must fail"
fi

# Refuses anything but a digest-addressed, tag-free image, and a well-formed repository.
for bad in "latest" "sha256:abc" "sha256:$(printf 'A%.0s' {1..64})" "sha256:$(printf 'a%.0s' {1..63})" ""; do
  if run bash "$script" --image "$image" --digest "$bad" --repository "$repository" >/dev/null 2>&1; then
    fail "digest '$bad' should be refused"
  fi
  [[ ! -s $tmpdir/log ]] || fail "cosign must not run for digest '$bad'"
done
for bad in "$image:1.2" "$image@$digest" "" "UPPER/case" "has space/x"; do
  if run bash "$script" --image "$bad" --digest "$digest" --repository "$repository" >/dev/null 2>&1; then
    fail "image '$bad' should be refused"
  fi
  [[ ! -s $tmpdir/log ]] || fail "cosign must not run for image '$bad'"
done
for bad in "no-slash" "a/b/c" "a b/c" ""; do
  if run bash "$script" --image "$image" --digest "$digest" --repository "$bad" >/dev/null 2>&1; then
    fail "repository '$bad' should be refused"
  fi
done
if run bash "$script" --image "$image" >/dev/null 2>&1; then fail "missing arguments should be refused"; fi

printf 'sign-image tests passed\n'
