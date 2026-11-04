#!/usr/bin/env bash
#
# Decides what to do when a GitHub Release already has a release-metadata.json asset.
#
# Prints one word on stdout:
#   keep     the existing asset is byte-identical to the regenerated file; leave it alone.
#   replace  the release is still a draft and the files differ only in the git-sync fields
#            (git_sync_release_id, git_sync_image_digest), and only between null (not
#            published) and a digest; the caller may replace the asset. A draft has no
#            consumers, and this keeps a re-run after a failed git-sync job from wedging the
#            release.
# Exits 1 (printing the reason to stderr) for anything else: a published release is never
# changed, a difference in release_id, chart_version or connector_image_digest is a conflict,
# and so is a change from one git-sync digest to a different one.

set -euo pipefail

usage() {
  cat >&2 <<'EOF'
Usage: reconcile-release-metadata.sh --existing <path> --generated <path> --draft <true|false>
EOF
  exit 2
}

fail() {
  printf 'release-metadata.json conflict: %s\n' "$1" >&2
  exit 1
}

existing=
generated=
draft=

while (($# > 0)); do
  case $1 in
    --existing)
      existing=${2-}
      shift 2
      ;;
    --generated)
      generated=${2-}
      shift 2
      ;;
    --draft)
      draft=${2-}
      shift 2
      ;;
    *)
      usage
      ;;
  esac
done

[[ -n $existing && -n $generated && ($draft == true || $draft == false) ]] || usage
[[ -f $existing ]] || fail "existing asset file does not exist: $existing"
[[ -f $generated ]] || fail "regenerated file does not exist: $generated"

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

if cmp -s "$existing" "$generated"; then
  printf 'keep\n'
  exit 0
fi

[[ $draft == true ]] || fail "the release is published, so the asset must not change"
bash "$root/scripts/validate-release-metadata.sh" "$existing" ||
  fail "the existing asset is not valid release metadata"
bash "$root/scripts/validate-release-metadata.sh" "$generated" ||
  fail "the regenerated file is not valid release metadata"

# Only the git-sync fields may differ.
for field in schema_version release_id chart_version connector_image_digest; do
  [[ $(jq -c --arg f "$field" '.[$f]' "$existing") == "$(jq -c --arg f "$field" '.[$f]' "$generated")" ]] ||
    fail "$field differs between the existing asset and the regenerated file"
done

# ...and only between "not published" (null) and "published" (a digest). Both git-sync fields
# change together (the validator requires them to be both null or both set). Changing one
# published digest into another would mean a release tag was overwritten, which never happens.
existing_has_git_sync=$(jq -r '.git_sync_image_digest != null' "$existing")
generated_has_git_sync=$(jq -r '.git_sync_image_digest != null' "$generated")
[[ $existing_has_git_sync != "$generated_has_git_sync" ]] ||
  fail "git_sync_image_digest changes from one image digest to another, which means a release tag changed"

printf 'replace\n'
