#!/usr/bin/env bash

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
generator="$root/scripts/release-metadata-json.sh"
reconcile="$root/scripts/reconcile-release-metadata.sh"
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

digest_a=sha256:0000000000000000000000000000000000000000000000000000000000000001
digest_b=sha256:0000000000000000000000000000000000000000000000000000000000000002
digest_c=sha256:0000000000000000000000000000000000000000000000000000000000000003

test_count=0

# make <name> <release id> <chart version> <connector digest> [git-sync digest]
make() {
  local name=$1
  local args=(--release-id "$2" --chart-version "$3" --connector-image-digest "$4" --output "$tmpdir/$name.json")
  if [[ -n ${5-} ]]; then
    args+=(--git-sync-image-digest "$5")
  fi
  bash "$generator" "${args[@]}"
}

# expect <expected stdout> <existing> <generated> <draft>
expect() {
  local out
  out=$(bash "$reconcile" --existing "$tmpdir/$2.json" --generated "$tmpdir/$3.json" --draft "$4" 2>"$tmpdir/stderr") || {
    printf 'expected %s vs %s (draft=%s) to give "%s", but it failed:\n' "$2" "$3" "$4" "$1" >&2
    cat "$tmpdir/stderr" >&2
    exit 1
  }
  if [[ $out != "$1" ]]; then
    printf 'expected %s vs %s (draft=%s) to give "%s", got "%s"\n' "$2" "$3" "$4" "$1" "$out" >&2
    exit 1
  fi
  test_count=$((test_count + 1))
}

# expect_conflict <existing> <generated> <draft> [message fragment]
expect_conflict() {
  if bash "$reconcile" --existing "$tmpdir/$1.json" --generated "$tmpdir/$2.json" --draft "$3" >"$tmpdir/stdout" 2>"$tmpdir/stderr"; then
    printf 'expected %s vs %s (draft=%s) to be a conflict\n' "$1" "$2" "$3" >&2
    exit 1
  fi
  if [[ -s $tmpdir/stdout ]]; then
    printf 'a conflict must not print an action, got: %s\n' "$(cat "$tmpdir/stdout")" >&2
    exit 1
  fi
  if [[ -n ${4-} ]] && ! grep -Fq -- "$4" "$tmpdir/stderr"; then
    printf 'expected "%s" in the conflict message, got:\n' "$4" >&2
    cat "$tmpdir/stderr" >&2
    exit 1
  fi
  test_count=$((test_count + 1))
}

make without-git-sync 20260919.0 1.2.3 "$digest_a"
make with-git-sync 20260919.0 1.2.3 "$digest_a" "$digest_b"
make with-other-git-sync 20260919.0 1.2.3 "$digest_a" "$digest_c"
make other-release 20260919.1 1.2.3 "$digest_a" "$digest_b"
make other-chart 20260919.0 1.2.4 "$digest_a" "$digest_b"
make other-connector 20260919.0 1.2.3 "$digest_c" "$digest_b"
make other-connector-without-git-sync 20260919.0 1.2.3 "$digest_c"

# Identical files are kept, draft or not.
expect keep with-git-sync with-git-sync true
expect keep without-git-sync without-git-sync true
expect keep with-git-sync with-git-sync false

# A draft may change the git-sync fields from null to a digest and back.
expect replace without-git-sync with-git-sync true
expect replace with-git-sync without-git-sync true

# ...but only between null and a digest: one published digest never becomes another.
expect_conflict with-git-sync with-other-git-sync true "from one image digest to another"
expect_conflict with-other-git-sync with-git-sync true "from one image digest to another"

# A published release is never changed.
expect_conflict without-git-sync with-git-sync false "published"
expect_conflict with-git-sync without-git-sync false "published"

# A difference in any release, chart or connector field is a conflict, even in a draft.
expect_conflict with-git-sync other-release true "release_id differs"
expect_conflict with-git-sync other-chart true "chart_version differs"
expect_conflict with-git-sync other-connector true "connector_image_digest differs"
expect_conflict without-git-sync other-connector-without-git-sync true "connector_image_digest differs"
expect_conflict with-git-sync other-connector-without-git-sync true "connector_image_digest differs"

# An invalid existing asset or regenerated file is never replaced.
printf '{"schema_version": 1}\n' >"$tmpdir/invalid.json"
expect_conflict invalid with-git-sync true "not valid release metadata"
expect_conflict with-git-sync invalid true "not valid release metadata"

# Usage errors.
for bad in "" maybe; do
  if bash "$reconcile" --existing "$tmpdir/with-git-sync.json" --generated "$tmpdir/with-git-sync.json" --draft "$bad" 2>/dev/null; then
    printf 'bad --draft value "%s" unexpectedly accepted\n' "$bad" >&2
    exit 1
  fi
done
if bash "$reconcile" --existing "$tmpdir/does-not-exist.json" --generated "$tmpdir/with-git-sync.json" --draft true 2>/dev/null; then
  printf 'missing existing file unexpectedly accepted\n' >&2
  exit 1
fi

printf 'release metadata reconcile tests passed (%s cases)\n' "$test_count"
