#!/usr/bin/env bash

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
script="$root/scripts/published-release-digests.sh"
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

release_id=20261114.0
connector_digest=sha256:$(printf 'a%.0s' {1..64})
git_sync_digest=sha256:$(printf 'b%.0s' {1..64})

# metadata <file> <release id> [git-sync digest]
metadata() {
  local file=$1 id=$2
  shift 2
  local args=(--release-id "$id" --chart-version 1.2.3 --connector-image-digest "$connector_digest" --output "$file")
  if (($# > 0)); then
    args+=(--git-sync-image-digest "$1")
  fi
  bash "$root/scripts/release-metadata-json.sh" "${args[@]}"
}

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# Both images published: both digests are printed.
metadata "$tmpdir/both.json" "$release_id" "$git_sync_digest"
out=$(bash "$script" --metadata "$tmpdir/both.json" --release-id "$release_id")
[[ $out == "connector_digest=$connector_digest"$'\n'"gitsync_digest=$git_sync_digest" ]] || fail "both digests: $out"

# No git-sync image: its digest is empty, never the string "null".
metadata "$tmpdir/connector-only.json" "$release_id"
out=$(bash "$script" --metadata "$tmpdir/connector-only.json" --release-id "$release_id")
[[ $out == "connector_digest=$connector_digest"$'\n'"gitsync_digest=" ]] || fail "connector only: $out"
if grep -q null <<<"$out"; then fail "null leaked into the output"; fi

# A file of another release is refused and prints nothing.
if out=$(bash "$script" --metadata "$tmpdir/both.json" --release-id 20261114.1 2>"$tmpdir/stderr"); then
  fail "another release's file accepted"
fi
[[ -z $out ]] || fail "output printed for another release: $out"
grep -q 'not 20261114.1' "$tmpdir/stderr" || fail "mismatch not explained"

# A file that is not valid release metadata is refused and prints nothing.
printf '{"release_id": "%s"}\n' "$release_id" >"$tmpdir/invalid.json"
if out=$(bash "$script" --metadata "$tmpdir/invalid.json" --release-id "$release_id" 2>/dev/null); then
  fail "invalid file accepted"
fi
[[ -z $out ]] || fail "output printed for an invalid file: $out"
if bash "$script" --metadata "$tmpdir/missing.json" --release-id "$release_id" >/dev/null 2>&1; then
  fail "missing file accepted"
fi

# Usage errors.
if bash "$script" --metadata "$tmpdir/both.json" >/dev/null 2>&1; then fail "missing release ID accepted"; fi
if bash "$script" --release-id "$release_id" >/dev/null 2>&1; then fail "missing metadata accepted"; fi

printf 'published release digest tests passed\n'
