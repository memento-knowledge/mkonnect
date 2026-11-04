#!/usr/bin/env bash

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
generator="$root/scripts/release-metadata-json.sh"
validator="$root/scripts/validate-release-metadata.sh"
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

connector_digest=sha256:0000000000000000000000000000000000000000000000000000000000000001
git_sync_digest=sha256:0000000000000000000000000000000000000000000000000000000000000002

# --- generator output is byte-exact ---

bash "$generator" --release-id 20260919.0 --chart-version 1.2.3 \
  --connector-image-digest "$connector_digest" \
  --git-sync-image-digest "$git_sync_digest" \
  --output "$tmpdir/full.json"
cat >"$tmpdir/full.expected" <<EOF
{
  "schema_version": 1,
  "release_id": "20260919.0",
  "chart_version": "1.2.3",
  "git_sync_release_id": "20260919.0",
  "connector_image_digest": "$connector_digest",
  "git_sync_image_digest": "$git_sync_digest"
}
EOF
cmp "$tmpdir/full.json" "$tmpdir/full.expected"

bash "$generator" --release-id 20260919.1 --chart-version 1.2.4 \
  --connector-image-digest "$connector_digest" \
  --output "$tmpdir/partial.json"
cat >"$tmpdir/partial.expected" <<EOF
{
  "schema_version": 1,
  "release_id": "20260919.1",
  "chart_version": "1.2.4",
  "git_sync_release_id": null,
  "connector_image_digest": "$connector_digest",
  "git_sync_image_digest": null
}
EOF
cmp "$tmpdir/partial.json" "$tmpdir/partial.expected"

if bash "$generator" --release-id 20260919.0 --chart-version 1.2.3 --output "$tmpdir/missing.json" 2>/dev/null; then
  printf 'generator unexpectedly accepted a missing connector digest\n' >&2
  exit 1
fi
test ! -e "$tmpdir/missing.json"

# --- validator ---

test_count=0

expect_valid() {
  local name=$1
  bash "$validator" "$tmpdir/$name.json" 2>"$tmpdir/$name.err" || {
    printf 'expected %s to be valid:\n' "$name" >&2
    cat "$tmpdir/$name.err" >&2
    exit 1
  }
  test_count=$((test_count + 1))
}

# expect_invalid <name> [expected message fragment]
expect_invalid() {
  local name=$1
  local fragment=${2-}
  if bash "$validator" "$tmpdir/$name.json" 2>"$tmpdir/$name.err"; then
    printf 'expected %s to be invalid\n' "$name" >&2
    exit 1
  fi
  if [[ -n $fragment ]] && ! grep -Fq -- "$fragment" "$tmpdir/$name.err"; then
    printf 'expected %s to fail with "%s", got:\n' "$name" "$fragment" >&2
    cat "$tmpdir/$name.err" >&2
    exit 1
  fi
  test_count=$((test_count + 1))
}

# write_case <name> <release_id> <chart_version> <git_sync_release_id> <connector_digest> <git_sync_digest>
# Values are inserted as raw JSON, so pass strings with their quotes and null bare.
write_case() {
  cat >"$tmpdir/$1.json" <<EOF
{
  "schema_version": 1,
  "release_id": $2,
  "chart_version": $3,
  "git_sync_release_id": $4,
  "connector_image_digest": $5,
  "git_sync_image_digest": $6
}
EOF
}

good_id='"20260919.0"'
good_chart='"1.2.3"'
good_connector="\"$connector_digest\""
good_git_sync="\"$git_sync_digest\""

cp "$tmpdir/full.json" "$tmpdir/valid-full.json"
cp "$tmpdir/partial.json" "$tmpdir/valid-partial.json"
write_case valid-prerelease-chart "$good_id" '"1.2.3-rc.1+build.5"' "$good_id" "$good_connector" "$good_git_sync"
write_case valid-high-index '"20260919.12"' "$good_chart" '"20260919.12"' "$good_connector" "$good_git_sync"
expect_valid valid-full
expect_valid valid-partial
expect_valid valid-prerelease-chart
expect_valid valid-high-index

# Nullability: only the two git-sync fields may be null, and only together.
write_case null-connector-digest "$good_id" "$good_chart" null null "$good_git_sync"
write_case null-release-id null "$good_chart" null "$good_connector" null
write_case null-chart-version "$good_id" null null "$good_connector" null
write_case only-git-sync-id-null "$good_id" "$good_chart" null "$good_connector" "$good_git_sync"
write_case only-git-sync-digest-null "$good_id" "$good_chart" "$good_id" "$good_connector" null
write_case git-sync-id-mismatch "$good_id" "$good_chart" '"20260919.1"' "$good_connector" "$good_git_sync"
expect_invalid null-connector-digest "connector_image_digest must be"
expect_invalid null-release-id "release_id must have"
expect_invalid null-chart-version "chart_version must be"
expect_invalid only-git-sync-id-null "both null or both set"
expect_invalid only-git-sync-digest-null "both null or both set"
expect_invalid git-sync-id-mismatch "must be null or equal release_id"

# Digest form.
bad_digests=(
  '"sha256:ABCDEF0000000000000000000000000000000000000000000000000000000001"'
  '"sha256:00000000000000000000000000000000000000000000000000000000000001"'
  '"sha256:00000000000000000000000000000000000000000000000000000000000000001"'
  '"0000000000000000000000000000000000000000000000000000000000000001"'
  '"sha512:0000000000000000000000000000000000000000000000000000000000000001"'
  '"sha256:000000000000000000000000000000000000000000000000000000000000000g"'
  '""'
  '1'
)
index=0
for digest in "${bad_digests[@]}"; do
  write_case "bad-connector-digest-$index" "$good_id" "$good_chart" "$good_id" "$digest" "$good_git_sync"
  write_case "bad-git-sync-digest-$index" "$good_id" "$good_chart" "$good_id" "$good_connector" "$digest"
  expect_invalid "bad-connector-digest-$index"
  expect_invalid "bad-git-sync-digest-$index"
  index=$((index + 1))
done

# Release ID form.
bad_ids=('"20260919"' '"2026-09-19.0"' '"20260919.00"' '"20260919.01"' '"20260919."' '"20260919.0.1"' '"v20260919.0"' '"20260919.0\n"' '20260919' '""')
index=0
for id in "${bad_ids[@]}"; do
  write_case "bad-release-id-$index" "$id" "$good_chart" null "$good_connector" null
  expect_invalid "bad-release-id-$index"
  index=$((index + 1))
done

# Chart version form.
for chart in '"1.2"' '"v1.2.3"' '"01.2.3"' '""' '123'; do
  write_case "bad-chart-version-$index" "$good_id" "$chart" null "$good_connector" null
  expect_invalid "bad-chart-version-$index"
  index=$((index + 1))
done

# schema_version.
for version in 0 2 '"1"' 1.5 null; do
  cat >"$tmpdir/bad-schema-version-$index.json" <<EOF
{
  "schema_version": $version,
  "release_id": "20260919.0",
  "chart_version": "1.2.3",
  "git_sync_release_id": null,
  "connector_image_digest": "$connector_digest",
  "git_sync_image_digest": null
}
EOF
  expect_invalid "bad-schema-version-$index"
  index=$((index + 1))
done

# Keys: missing, extra, renamed, and reordered.
jq 'del(.git_sync_image_digest)' "$tmpdir/valid-partial.json" >"$tmpdir/missing-key.json"
jq 'del(.schema_version)' "$tmpdir/valid-partial.json" >"$tmpdir/missing-schema-version.json"
jq '. + {extra: "value"}' "$tmpdir/valid-partial.json" >"$tmpdir/extra-key.json"
jq 'with_entries(if .key == "release_id" then .key = "releaseId" else . end)' "$tmpdir/valid-partial.json" >"$tmpdir/renamed-key.json"
jq '{release_id, schema_version, chart_version, git_sync_release_id, connector_image_digest, git_sync_image_digest}' \
  "$tmpdir/valid-partial.json" >"$tmpdir/reordered-keys.json"
expect_invalid missing-key "keys must be exactly"
expect_invalid missing-schema-version "keys must be exactly"
expect_invalid extra-key "keys must be exactly"
expect_invalid renamed-key "keys must be exactly"
expect_invalid reordered-keys "canonical layout"

# Layout: compact, no trailing newline, duplicate key, extra document, and indentation.
jq -c . "$tmpdir/valid-partial.json" >"$tmpdir/compact.json"
printf '%s' "$(cat "$tmpdir/valid-partial.json")" >"$tmpdir/no-trailing-newline.json"
{
  sed '$d' "$tmpdir/valid-partial.json"
  printf '  ,"chart_version": "9.9.9"\n}\n'
} >"$tmpdir/duplicate-key.json"
cat "$tmpdir/valid-partial.json" "$tmpdir/valid-partial.json" >"$tmpdir/two-documents.json"
jq --tab . "$tmpdir/valid-partial.json" >"$tmpdir/tab-indent.json"
expect_invalid compact "canonical layout"
expect_invalid no-trailing-newline "canonical layout"
expect_invalid duplicate-key "canonical layout"
expect_invalid two-documents
expect_invalid tab-indent "canonical layout"

# Not a valid JSON object, not UTF-8, empty, or missing.
printf '%s\n' '[1, 2, 3]' >"$tmpdir/array.json"
printf '%s\n' '"text"' >"$tmpdir/string.json"
printf '%s\n' '{"schema_version": 1,' >"$tmpdir/truncated.json"
printf '// comment\n' | cat - "$tmpdir/valid-partial.json" >"$tmpdir/comment.json"
: >"$tmpdir/empty.json"
{
  sed '$d' "$tmpdir/valid-partial.json"
  printf '  ,"note": "\377"\n}\n'
} >"$tmpdir/invalid-utf8.json"
expect_invalid array "not a JSON object"
expect_invalid string "not a JSON object"
expect_invalid truncated
expect_invalid comment
expect_invalid empty
expect_invalid invalid-utf8 "not valid UTF-8"

if bash "$validator" "$tmpdir/does-not-exist.json" 2>/dev/null; then
  printf 'missing file unexpectedly validated\n' >&2
  exit 1
fi
if bash "$validator" 2>/dev/null; then
  printf 'validator without a file argument unexpectedly succeeded\n' >&2
  exit 1
else
  test $? -eq 2
fi

printf 'release metadata JSON tests passed (%s validator cases)\n' "$test_count"
