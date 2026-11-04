#!/usr/bin/env bash
#
# Writes release-metadata.json, the machine-readable summary attached to every GitHub Release.
#
# This script is the single place that knows the file's layout (key names, key order, and
# formatting). validate-release-metadata.sh re-renders a file through this script to check it,
# so changing the format means changing this script and that script's rules, nothing else.
#
# The git-sync fields are null when the git-sync image was not published for the release.

set -euo pipefail

usage() {
  cat >&2 <<'EOF'
Usage: release-metadata-json.sh --release-id <id> --chart-version <version> \
         --connector-image-digest <digest> [--git-sync-image-digest <digest>] --output <path>

Without --git-sync-image-digest, git_sync_release_id and git_sync_image_digest are written as null.
EOF
  exit 2
}

release_id=
chart_version=
connector_digest=
git_sync_digest=
output=

while (($# > 0)); do
  case $1 in
    --release-id)
      release_id=${2-}
      shift 2
      ;;
    --chart-version)
      chart_version=${2-}
      shift 2
      ;;
    --connector-image-digest)
      connector_digest=${2-}
      shift 2
      ;;
    --git-sync-image-digest)
      git_sync_digest=${2-}
      shift 2
      ;;
    --output)
      output=${2-}
      shift 2
      ;;
    *)
      usage
      ;;
  esac
done

[[ -n $release_id && -n $chart_version && -n $connector_digest && -n $output ]] || usage

output_dir=$(dirname "$output")
[[ -d $output_dir ]] || {
  printf 'output directory does not exist: %s\n' "$output_dir" >&2
  exit 1
}
output_tmp=$(mktemp "$output_dir/.release-metadata-json.XXXXXX")
trap 'rm -f "$output_tmp"' EXIT

# jq keeps object keys in the order written here and pretty-prints one key per line with a
# trailing newline.
jq -n \
  --arg release_id "$release_id" \
  --arg chart_version "$chart_version" \
  --arg connector_digest "$connector_digest" \
  --arg git_sync_digest "$git_sync_digest" \
  '{
    schema_version: 1,
    release_id: $release_id,
    chart_version: $chart_version,
    git_sync_release_id: (if $git_sync_digest == "" then null else $release_id end),
    connector_image_digest: $connector_digest,
    git_sync_image_digest: (if $git_sync_digest == "" then null else $git_sync_digest end)
  }' >"$output_tmp"
mv "$output_tmp" "$output"
