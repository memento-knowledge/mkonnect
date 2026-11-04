#!/usr/bin/env bash
#
# Validates a release-metadata.json file. Exit status 0 means the file is well formed; 1 means it
# is not (the reason is printed to stderr); 2 means bad usage.
#
# Rules:
#   - valid UTF-8 JSON object with exactly the schema's keys (no missing or extra keys)
#   - schema_version is the number 1
#   - release_id has the YYYYMMDD.n form; chart_version is a SemVer string
#   - the connector image digest is sha256:<64 lowercase hex>
#   - the two git-sync fields are the only nullable ones, and are both null or both set; when
#     set, git_sync_release_id equals release_id and the digest has the sha256 form
#   - the file is byte-for-byte what release-metadata-json.sh would write for the same values,
#     which also fixes key order, formatting, and rejects duplicate keys

set -euo pipefail

usage() {
  cat >&2 <<'EOF'
Usage: validate-release-metadata.sh <release-metadata.json>
EOF
  exit 2
}

fail() {
  printf 'release-metadata.json is invalid: %s\n' "$1" >&2
  exit 1
}

(($# == 1)) || usage
file=$1
[[ -f $file ]] || fail "file does not exist: $file"

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

iconv -f UTF-8 -t UTF-8 "$file" >/dev/null 2>&1 || fail "file is not valid UTF-8"
jq -e 'type == "object"' "$file" >/dev/null 2>&1 || fail "file is not a JSON object"

# jq -e fails on a false/null result, so each test below is phrased as a positive assertion.
check() {
  local message=$1
  local filter=$2
  jq -e "$filter" "$file" >/dev/null 2>&1 || fail "$message"
}

check "keys must be exactly schema_version, release_id, chart_version, git_sync_release_id, connector_image_digest, git_sync_image_digest" \
  '(keys | sort) == ["chart_version", "connector_image_digest", "git_sync_image_digest", "git_sync_release_id", "release_id", "schema_version"]'
check "schema_version must be 1" '.schema_version == 1 and (.schema_version | type) == "number"'
check "release_id must have the YYYYMMDD.n form" \
  '(.release_id | type) == "string" and (.release_id | test("^[0-9]{8}[.](0|[1-9][0-9]*)$"))'
check "chart_version must be a SemVer string" \
  '(.chart_version | type) == "string" and (.chart_version | test("^(0|[1-9][0-9]*)[.](0|[1-9][0-9]*)[.](0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?([+][0-9A-Za-z.-]+)?$"))'
check "connector_image_digest must be sha256:<64 lowercase hex characters> and cannot be null" \
  '(.connector_image_digest | type) == "string" and (.connector_image_digest | test("^sha256:[0-9a-f]{64}$"))'
check "git_sync_release_id and git_sync_image_digest must be both null or both set" \
  '((.git_sync_release_id == null) == (.git_sync_image_digest == null))'
check "git_sync_release_id must be null or equal release_id" \
  '.git_sync_release_id == null or .git_sync_release_id == .release_id'
check "git_sync_image_digest must be null or sha256:<64 lowercase hex characters>" \
  '.git_sync_image_digest == null or ((.git_sync_image_digest | type) == "string" and (.git_sync_image_digest | test("^sha256:[0-9a-f]{64}$")))'

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

render_args=(
  --release-id "$(jq -r '.release_id' "$file")"
  --chart-version "$(jq -r '.chart_version' "$file")"
  --connector-image-digest "$(jq -r '.connector_image_digest' "$file")"
  --output "$tmpdir/canonical.json"
)
if jq -e '.git_sync_image_digest != null' "$file" >/dev/null; then
  render_args+=(--git-sync-image-digest "$(jq -r '.git_sync_image_digest' "$file")")
fi
bash "$root/scripts/release-metadata-json.sh" "${render_args[@]}"
cmp -s "$file" "$tmpdir/canonical.json" ||
  fail "file does not match the canonical layout (key order, formatting, duplicate keys, or trailing newline)"
