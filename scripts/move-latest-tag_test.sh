#!/usr/bin/env bash

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
script="$root/scripts/move-latest-tag.sh"
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

release_id=20261114.0
registry=registry.example.invalid/alias
repository=memento-example
image="$registry/$repository"
new_digest=sha256:$(printf 'a%.0s' {1..64})
old_digest=sha256:$(printf 'b%.0s' {1..64})
other_digest=sha256:$(printf 'c%.0s' {1..64})

# The stubs model a registry as one file per tag, "<digest> <version label>" (the label "-"
# stands for an image without one). Every call is appended to a log so the tests can assert what
# was (and was not) run, and in which order.
mkdir "$tmpdir/bin"
cat >"$tmpdir/bin/aws" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
ORIGINAL_ARGS="$*"
repository=
tag=
while (($# > 0)); do
  case $1 in
    --repository-name) repository=$2; shift 2 ;;
    --image-ids) tag=${2#imageTag=}; shift 2 ;;
    *) shift ;;
  esac
done
printf 'aws %s\n' "$ORIGINAL_ARGS" >>"$FAKE_LOG"
if [[ -n ${FAKE_AWS_DENY-} || ( -n ${FAKE_AWS_DENY_TAG-} && $tag == "$FAKE_AWS_DENY_TAG" ) ]]; then
  printf '%s\n' 'An error occurred (AccessDeniedException) when calling the DescribeImages operation' >&2
  exit 255
fi
record="$FAKE_REGISTRY/$repository/$tag"
if [[ -f $record ]]; then
  read -r digest _ <"$record"
  printf '{"imageDetails":[{"imageDigest":"%s"}]}\n' "$digest"
else
  printf '%s\n' 'An error occurred (ImageNotFoundException) when calling the DescribeImages operation' >&2
  exit 255
fi
EOF
cat >"$tmpdir/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$FAKE_LOG"

# The record of the image with the given digest in a repository directory, found by content: the
# same image can be reachable under several tags.
find_by_digest() {
  local candidate digest
  for candidate in "$1"/*; do
    [[ -f $candidate ]] || continue
    read -r digest _ <"$candidate"
    if [[ $digest == "$2" ]]; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done
  return 1
}
local_path() { printf '%s/%s\n' "$FAKE_LOCAL" "$(printf '%s' "$1" | tr '/:@' '___')"; }

case $1 in
  pull)
    # <registry>/<alias>/<repository>@<digest>
    ref=$2
    digest=${ref#*@}
    repo=${ref%@*}
    repo=${repo##*/}
    if [[ -n ${FAKE_PULL_FAIL_DIGEST-} && $digest == "$FAKE_PULL_FAIL_DIGEST" ]]; then
      # The pull fails although an unlabelled copy is cached locally, so a script that ignored the
      # failed pull would read "no label" and treat the image as older.
      printf '%s -\n' "$digest" >"$(local_path "$ref")"
      printf 'simulated pull failure\n' >&2
      exit 1
    fi
    record=$(find_by_digest "$FAKE_REGISTRY/$repo" "$digest") || { printf 'not found: %s\n' "$ref" >&2; exit 1; }
    cp "$record" "$(local_path "$ref")"
    ;;
  inspect)
    if [[ -n ${FAKE_INSPECT_FAIL_DIGEST-} && $2 == *"@$FAKE_INSPECT_FAIL_DIGEST" ]]; then
      printf 'simulated inspect failure\n' >&2
      exit 1
    fi
    read -r _ version <"$(local_path "$2")"
    case $4 in
      *org.opencontainers.image.version*)
        [[ $version == - ]] || printf '%s\n' "$version"
        [[ $version != - ]] || printf '\n'
        ;;
      *) exit 2 ;;
    esac
    ;;
  buildx)
    # buildx imagetools create --prefer-index=false --tag <new ref> <repo>@<digest>
    [[ $2 == imagetools && $3 == create && $4 == --prefer-index=false && $5 == --tag ]] || {
      printf 'unexpected buildx command: %s\n' "$*" >&2
      exit 2
    }
    new_ref=$6
    source=$7
    if [[ -n ${FAKE_CREATE_FAIL-} ]]; then
      printf 'simulated create failure\n' >&2
      exit 1
    fi
    source_repo=${source%@*}
    source_digest=${source#*@}
    repo_dir="$FAKE_REGISTRY/${source_repo##*/}"
    record=$(find_by_digest "$repo_dir" "$source_digest") || { printf 'no such source digest\n' >&2; exit 1; }
    # FAKE_CREATE_NOOP models a write that the registry accepted but did not apply.
    [[ -n ${FAKE_CREATE_NOOP-} ]] || cp "$record" "$repo_dir/${new_ref##*:}"
    ;;
  *)
    printf 'unexpected docker command: %s\n' "$*" >&2
    exit 2
    ;;
esac
EOF
chmod +x "$tmpdir/bin/aws" "$tmpdir/bin/docker"

test_count=0

aws_call() {
  printf 'aws ecr-public describe-images --region us-east-1 --repository-name %s --image-ids imageTag=%s --output json\n' "$repository" "$1"
}
pull_call() { printf 'pull %s@%s\n' "$image" "$1"; }
inspect_call() {
  printf 'inspect %s@%s --format {{ with .Config.Labels }}{{ index . "org.opencontainers.image.version" }}{{ end }}\n' "$image" "$1"
}
create_call() { printf 'buildx imagetools create --prefer-index=false --tag %s:latest %s@%s\n' "$image" "$image" "$1"; }

new_case() {
  case_dir="$tmpdir/$1"
  mkdir -p "$case_dir/registry/$repository" "$case_dir/local"
  : >"$case_dir/log"
}

# seed <tag> <digest> <version label or ->
seed() {
  printf '%s %s\n' "$2" "$3" >"$case_dir/registry/$repository/$1"
}
# The release being promoted, as the publish job leaves it.
seed_release() { seed "$release_id" "$new_digest" "$release_id"; }

run_move() {
  FAKE_REGISTRY="$case_dir/registry" FAKE_LOCAL="$case_dir/local" FAKE_LOG="$case_dir/log" \
    PATH="$tmpdir/bin:$PATH" \
    bash "$script" \
    --registry "$registry" \
    --repository "$repository" \
    --release-id "${RELEASE_ID_ARG-$release_id}" \
    --digest "${DIGEST_ARG-$new_digest}"
}

expect_success() {
  run_move >"$case_dir/stdout" 2>"$case_dir/stderr" || {
    printf 'expected %s to succeed:\n' "$case_dir" >&2
    cat "$case_dir/stderr" >&2
    exit 1
  }
  test_count=$((test_count + 1))
}

expect_failure() {
  if run_move >"$case_dir/stdout" 2>"$case_dir/stderr"; then
    printf 'expected %s to fail\n' "$case_dir" >&2
    exit 1
  fi
  test_count=$((test_count + 1))
}

registry_digest() { awk '{ print $1 }' "$case_dir/registry/$repository/$1"; }
registry_lacks() { test ! -e "$case_dir/registry/$repository/$1"; }
log_has() { grep -Eq -- "$1" "$case_dir/log"; }
log_lacks() { ! grep -Eq -- "$1" "$case_dir/log"; }
output_has() { cat "$case_dir/stdout" "$case_dir/stderr" | grep -Fq -- "$1"; }
no_write() { log_lacks '^buildx '; }

# expect_moves <case> <version label of the current latest, or -> [<release id>]: the current
# `latest` is replaced by the release. expect_stays: it is left alone and nothing is written.
expect_moves() {
  new_case "$1"
  seed latest "$old_digest" "$2"
  RELEASE_ID_ARG=${3:-$release_id}
  seed "$RELEASE_ID_ARG" "$new_digest" "$RELEASE_ID_ARG"
  expect_success
  test "$(registry_digest latest)" = "$new_digest"
  log_has '^buildx imagetools create '
  unset RELEASE_ID_ARG
}
expect_stays() {
  new_case "$1"
  seed latest "$old_digest" "$2"
  RELEASE_ID_ARG=${3:-$release_id}
  seed "$RELEASE_ID_ARG" "$new_digest" "$RELEASE_ID_ARG"
  expect_success
  test "$(registry_digest latest)" = "$old_digest"
  no_write
  unset RELEASE_ID_ARG
}

# 1. No `latest` yet (a first git-sync release): it is created, and the exact calls are pinned:
# release tag lookup, the candidate's labels, the `latest` lookup, the copy by digest, the re-check.
new_case fresh
seed_release
expect_success
test "$(registry_digest latest)" = "$new_digest"
{
  aws_call "$release_id"
  aws_call latest
  pull_call "$new_digest"
  inspect_call "$new_digest"
  create_call "$new_digest"
  aws_call latest
} >"$case_dir/expected-calls"
cmp "$case_dir/expected-calls" "$case_dir/log"
output_has 'does not exist yet'

# 2. Re-running the same release changes nothing, and pulls nothing.
: >"$case_dir/log"
expect_success
no_write
log_lacks '^(pull|inspect) '
test "$(registry_digest latest)" = "$new_digest"
output_has 'nothing to do'

# 3. A stale `latest` without a version label is replaced, and the log says why.
expect_moves unlabelled -
output_has 'not a calendar release ID'

# 4. A version label that is not a calendar release ID is treated the same way.
for label in v1.2 latest 2026-09-19 20260919 20260919.0.1 20260919.x 2026091.0 20260919.00 20260919.1234567 '<no value>' ""; do
  new_case "non-calendar-$label"
  seed latest "$old_digest" "${label:--}"
  seed_release
  expect_success
  test "$(registry_digest latest)" = "$new_digest"
  output_has 'not a calendar release ID'
done

# 5. An older release is replaced.
expect_moves older 20260919.0
output_has 'moving it forward'
expect_moves older-same-day 20261114.0 20261114.1

# 6. Release IDs compare numerically: index 10 is newer than 9 (not older, as a text comparison
# would say). Only canonical IDs (no leading zeros in the index, as release-metadata.sh requires)
# are calendar IDs, so a padded label is not compared as a number (and never read as octal).
expect_moves index-ten 20261114.9 20261114.10
expect_moves next-month 20261114.9 20261201.0
expect_moves index-zero 20261113.12 20261114.0
expect_stays index-nine-is-older 20261114.10 20261114.9
expect_moves index-padded-is-not-calendar 20261114.09 20261114.10
output_has 'not a calendar release ID'

# 7. Never backwards: re-running an older release leaves a newer `latest` alone and succeeds.
expect_stays rerun-older 20261201.0 20261114.0
output_has 'not moving it backwards'
expect_stays rerun-earlier-same-day 20261114.3 20261114.1
expect_stays rerun-earlier-year 20270101.0 20261231.7

# 8. `latest` that carries this release ID under a different digest is a conflict.
new_case same-id-other-digest
seed_release
seed latest "$other_digest" "$release_id"
expect_failure
no_write
test "$(registry_digest latest)" = "$other_digest"

# 9. Only a consistently published release can become `latest`.
new_case release-tag-missing
expect_failure
no_write
registry_lacks latest
output_has 'does not exist'

new_case release-tag-other-digest
seed "$release_id" "$other_digest" "$release_id"
expect_failure
no_write
registry_lacks latest
output_has "$other_digest"

new_case release-label-mismatch
seed "$release_id" "$new_digest" 20260101.0
expect_failure
no_write
registry_lacks latest

new_case release-label-missing
seed "$release_id" "$new_digest" -
expect_failure
no_write
registry_lacks latest

# 10. A registry error stops the run before anything is written.
new_case registry-denied
seed_release
FAKE_AWS_DENY=1 expect_failure
no_write
registry_lacks latest
unset FAKE_AWS_DENY

# A registry error on the `latest` lookup alone (the release tag resolved) is not read as "missing".
new_case latest-lookup-denied
seed_release
seed latest "$old_digest" 20261201.0
FAKE_AWS_DENY_TAG=latest expect_failure
no_write
test "$(registry_digest latest)" = "$old_digest"
output_has AccessDenied
unset FAKE_AWS_DENY_TAG

# The release tag lookup failing for a reason other than "not found" fails the same way.
new_case release-lookup-denied
seed_release
FAKE_AWS_DENY_TAG=$release_id expect_failure
no_write
registry_lacks latest
output_has AccessDenied
unset FAKE_AWS_DENY_TAG

# 11. An existing `latest` that cannot be inspected is not guessed to be older.
new_case current-not-pullable
seed_release
seed latest "$old_digest" 20260919.0
FAKE_PULL_FAIL_DIGEST=$old_digest expect_failure
no_write
test "$(registry_digest latest)" = "$old_digest"

# An uninspectable `latest` (the pull worked) is not read as "no label" either.
new_case current-not-inspectable
seed_release
seed latest "$old_digest" 20260919.0
FAKE_INSPECT_FAIL_DIGEST=$old_digest expect_failure
no_write
test "$(registry_digest latest)" = "$old_digest"
unset FAKE_INSPECT_FAIL_DIGEST

new_case candidate-not-inspectable
seed_release
FAKE_INSPECT_FAIL_DIGEST=$new_digest expect_failure
no_write
registry_lacks latest
unset FAKE_INSPECT_FAIL_DIGEST

new_case candidate-not-pullable
seed_release
FAKE_PULL_FAIL_DIGEST=$new_digest expect_failure
no_write
registry_lacks latest

# 12. A failed write fails the script; so does a write that did not take effect.
new_case create-fails
seed_release
FAKE_CREATE_FAIL=1 expect_failure
registry_lacks latest
unset FAKE_CREATE_FAIL

new_case create-has-no-effect
seed_release
seed latest "$old_digest" 20260919.0
FAKE_CREATE_NOOP=1 expect_failure
test "$(registry_digest latest)" = "$old_digest"
output_has "resolves to $old_digest after the update"
unset FAKE_CREATE_NOOP

# 13. Only latest is written: the release tag and the commit alias are never touched.
new_case release-tag-untouched
seed_release
seed "sha-0123456789abcdef0123456789abcdef01234567" "$new_digest" "$release_id"
expect_success
test "$(registry_digest "$release_id")" = "$new_digest"
test "$(registry_digest "sha-0123456789abcdef0123456789abcdef01234567")" = "$new_digest"
test "$(find "$case_dir/registry/$repository" -type f | wc -l | tr -d ' ')" -eq 3

# 14. Usage errors are rejected before the registry is contacted.
new_case usage
if bash "$script" --repository "$repository" 2>/dev/null; then
  printf 'incomplete arguments unexpectedly accepted\n' >&2
  exit 1
fi
for bad_id in latest 20261114 2026-11-14 2026111.0 20261114.0.1 20261114.a 20261114.00 20261114.01 "20261114.0 " ""; do
  RELEASE_ID_ARG=$bad_id
  expect_failure
  unset RELEASE_ID_ARG
  test ! -s "$case_dir/log"
done
for bad_digest in latest sha256:abc "sha256:$(printf 'A%.0s' {1..64})" "sha256:$(printf 'a%.0s' {1..63})" "$repository@$new_digest" ""; do
  DIGEST_ARG=$bad_digest
  expect_failure
  unset DIGEST_ARG
  test ! -s "$case_dir/log"
done

printf 'latest tag move tests passed (%s cases)\n' "$test_count"
