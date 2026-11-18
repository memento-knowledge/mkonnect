#!/usr/bin/env bash
#
# Reads the image digests of a published release from its release-metadata.json, the file that
# consumers read, and prints them as `connector_digest=<digest>` and `gitsync_digest=<digest>` lines
# (suitable for appending to $GITHUB_OUTPUT). `gitsync_digest` is empty when the git-sync image was
# not published for the release (its field is null).
#
# Nothing is printed unless the file passes scripts/validate-release-metadata.sh and its release_id
# is --release-id, so a file of another release can never be used. Exit status 1 means the file is
# not usable (the reason is on stderr); 2 means bad usage.

set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: published-release-digests.sh --metadata <release-metadata.json> --release-id <YYYYMMDD.n>
USAGE
  exit 2
}

metadata=
release_id=

while (($# > 0)); do
  case $1 in
    --metadata)
      metadata=${2-}
      shift 2
      ;;
    --release-id)
      release_id=${2-}
      shift 2
      ;;
    *)
      usage
      ;;
  esac
done

[[ -n $metadata && -n $release_id ]] || usage

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

bash "$root/scripts/validate-release-metadata.sh" "$metadata"
file_release_id=$(jq -r .release_id "$metadata")
if [ "$file_release_id" != "$release_id" ]; then
  printf 'release-metadata.json is for release %s, not %s\n' "$file_release_id" "$release_id" >&2
  exit 1
fi

printf 'connector_digest=%s\n' "$(jq -r .connector_image_digest "$metadata")"
printf 'gitsync_digest=%s\n' "$(jq -r '.git_sync_image_digest // empty' "$metadata")"
