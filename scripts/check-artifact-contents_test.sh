#!/usr/bin/env bash
#
# Tests scripts/check-artifact-contents.sh, including its negative cases: a check that cannot fail
# proves nothing, so each kind of artifact is also given a deliberately bad copy that must be refused.
#
# The chart cases need helm (they are skipped without it). The image cases at the end need a Docker
# daemon and run only with CHECK_ARTIFACT_DOCKER=1 (CI sets it in the image job).

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
check="$root/scripts/check-artifact-contents.sh"
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

pass() { printf 'ok   %s\n' "$1"; }
die() {
  printf 'FAIL %s\n' "$*" >&2
  exit 1
}

expect_ok() {
  local name=$1
  shift
  bash "$check" "$@" >"$tmpdir/out" 2>&1 || {
    cat "$tmpdir/out" >&2
    die "$name: expected the check to pass"
  }
  pass "$name"
}

# expect_fail NAME PATTERN ARGS...: the check must fail and mention PATTERN.
expect_fail() {
  local name=$1 pattern=$2
  shift 2
  if bash "$check" "$@" >"$tmpdir/out" 2>&1; then
    cat "$tmpdir/out" >&2
    die "$name: expected the check to fail"
  fi
  grep -q -- "$pattern" "$tmpdir/out" || {
    cat "$tmpdir/out" >&2
    die "$name: failed, but not with a message containing '$pattern'"
  }
  pass "$name (refused)"
}

# --- image filesystem listings ---------------------------------------------------------------------
good="$tmpdir/good.lst"
cat >"$good" <<'EOF'
.dockerenv
bin/
bin/sh
data/
etc/
etc/profile.d/20locale.sh
etc/ssl/certs/ca-certificates.crt
mkonnect
tmp/
usr/
usr/bin/
usr/bin/test
usr/lib/
EOF
expect_ok "a listing with only the binary, /data and system files" listing "$good" /mkonnect

with_extra() {
  cp "$good" "$tmpdir/bad.lst"
  printf '%s\n' "$@" >>"$tmpdir/bad.lst"
}

for bad in \
  "usr/local/foo_test.go" \
  "usr/local/main.go" \
  "usr/share/testdata/case.txt" \
  "opt/app/testdata/" \
  ".git/config" \
  "opt/repo/.git/HEAD" \
  "usr/local/replicarig_test.go" \
  "usr/share/RIG.md" \
  "opt/scripts/release.sh" \
  "opt/go.mod" \
  "opt/Dockerfile" \
  "usr/local/foo_test.yaml"; do
  with_extra "$bad"
  expect_fail "a listing that also contains $bad" "must not be shipped" listing "$tmpdir/bad.lst" /mkonnect
done

with_extra "src/"
expect_fail "a listing with an unexpected top-level directory" "unexpected top-level entry" listing "$tmpdir/bad.lst" /mkonnect

with_extra "data/key"
expect_fail "a listing with a non-empty /data" "must ship empty" listing "$tmpdir/bad.lst" /mkonnect

grep -v '^mkonnect$' "$good" >"$tmpdir/nobinary.lst"
expect_fail "a listing without the binary" "does not contain the binary" listing "$tmpdir/nobinary.lst" /mkonnect

# --- Dockerfiles -----------------------------------------------------------------------------------
expect_ok "the connector Dockerfile" dockerfile "$root/Dockerfile" /mkonnect /data-empty
expect_ok "the git-sync Dockerfile" dockerfile "$root/Dockerfile.gitsync" /mkonnect-gitsync

dockerfile_with() {
  sed -e '/^ENTRYPOINT/i\
'"$1" "$root/Dockerfile" >"$tmpdir/Dockerfile.bad"
}
dockerfile_with 'COPY foo_test.go /foo_test.go'
expect_fail "a final stage copying from the build context" "may only COPY --from=builder" dockerfile "$tmpdir/Dockerfile.bad" /mkonnect /data-empty
dockerfile_with 'COPY --from=builder /src /src'
expect_fail "a final stage copying the whole source tree" "not one of" dockerfile "$tmpdir/Dockerfile.bad" /mkonnect /data-empty
dockerfile_with 'ADD . /app'
expect_fail "a final stage using ADD" "must not use ADD" dockerfile "$tmpdir/Dockerfile.bad" /mkonnect /data-empty

# --- release assets --------------------------------------------------------------------------------
assets="$tmpdir/assets"
mkdir "$assets"
touch "$assets/mkonnect-1.2.3.tgz" "$assets/mkonnect-1.2.3.tgz.sha256"
expect_ok "release assets: chart and checksum" assets "$assets" mkonnect 1.2.3
touch "$assets/release-metadata.json"
expect_ok "release assets: chart, checksum and metadata" assets "$assets" mkonnect 1.2.3
touch "$assets/replicarig_test.go"
expect_fail "release assets with a stray file" "unexpected release asset" assets "$assets" mkonnect 1.2.3
rm "$assets/replicarig_test.go" "$assets/mkonnect-1.2.3.tgz.sha256"
expect_fail "release assets without the checksum" "lack" assets "$assets" mkonnect 1.2.3

# --- Helm chart ------------------------------------------------------------------------------------
if command -v helm >/dev/null 2>&1; then
  chart="$tmpdir/chart"
  mkdir "$tmpdir/pkg"
  cp -R "$root/charts/mkonnect" "$chart"
  helm package "$chart" --destination "$tmpdir/pkg" >/dev/null
  package=$(ls "$tmpdir"/pkg/*.tgz)
  expect_ok "the packaged chart" chart "$package"

  # helm package leaves out what .helmignore names, so a test file placed in the chart is dropped...
  : >"$chart/templates/foo_test.yaml"
  rm -f "$tmpdir"/pkg/*.tgz
  helm package "$chart" --destination "$tmpdir/pkg" >/dev/null
  expect_ok "a test file in the chart is dropped by .helmignore" chart "$(ls "$tmpdir"/pkg/*.tgz)"
  # ...but a package built without that protection (or one that names a test file differently) is
  # refused.
  rm "$chart/templates/foo_test.yaml"
  for bad in "templates/foo_test.yaml" "tests/rig.txt" "templates/tests/probe.yaml" "testdata/x.yaml" "templates/hook.sh" ".git/config" "rig_test.go"; do
    rm -rf "$tmpdir/raw"
    mkdir -p "$tmpdir/raw"
    cp -R "$chart" "$tmpdir/raw/mkonnect"
    mkdir -p "$tmpdir/raw/mkonnect/$(dirname "$bad")"
    : >"$tmpdir/raw/mkonnect/$bad"
    tar -czf "$tmpdir/raw.tgz" -C "$tmpdir/raw" mkonnect
    expect_fail "a chart package that also contains $bad" "must not be shipped" chart "$tmpdir/raw.tgz"
  done
else
  printf 'skip the Helm chart cases: helm is not installed\n'
fi

# --- container images (opt-in) ---------------------------------------------------------------------
if [[ ${CHECK_ARTIFACT_DOCKER:-} == 1 ]]; then
  ctx="$tmpdir/ctx"
  mkdir -p "$ctx/data-empty"
  printf 'binary' >"$ctx/mkonnect"
  printf 'package x\n' >"$ctx/foo_test.go"
  cat >"$ctx/Dockerfile.good" <<'EOF'
FROM scratch
COPY mkonnect /mkonnect
COPY data-empty /data
EOF
  cat >"$ctx/Dockerfile.bad" <<'EOF'
FROM scratch
COPY mkonnect /mkonnect
COPY data-empty /data
COPY foo_test.go /foo_test.go
EOF
  docker build -q -f "$ctx/Dockerfile.good" -t artifact-check-good:test "$ctx" >/dev/null
  docker build -q -f "$ctx/Dockerfile.bad" -t artifact-check-bad:test "$ctx" >/dev/null
  trap 'rm -rf "$tmpdir"; docker rmi -f artifact-check-good:test artifact-check-bad:test >/dev/null 2>&1 || true' EXIT
  expect_ok "a built image with only the binary and /data" image artifact-check-good:test /mkonnect
  expect_fail "a built image that also holds a test file" "foo_test.go" image artifact-check-bad:test /mkonnect
else
  printf 'skip the image cases: CHECK_ARTIFACT_DOCKER is not 1\n'
fi

printf 'all artifact content checks behave as expected\n'
