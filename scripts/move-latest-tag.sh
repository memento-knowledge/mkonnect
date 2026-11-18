#!/usr/bin/env bash
#
# Moves the `latest` tag of an Amazon ECR Public repository forward to a release that has already
# been published, and never backwards. `latest` is a convenience tag for first installs; release-ID
# tags are never touched by this script.
#
# The release is identified by its release ID (YYYYMMDD.n) and the digest the release workflow
# recorded for it. The script moves `latest` only when all of these hold:
#   - the release tag <release-id> resolves to --digest;
#   - `latest` does not exist, or the image it points to is older than this release. "Older"
#     compares the calendar release IDs numerically (date, then index) using the
#     org.opencontainers.image.version label of the image currently tagged `latest`. A `latest`
#     whose version label is missing or is not a calendar release ID is treated as older and is
#     replaced; the log says so;
#   - just before the write, the image at --digest carries the version label <release-id>.
#
# Outcomes. Exit 0, nothing written:
#   - `latest` already is --digest;
#   - `latest` is a newer release (a notice is logged), so re-running an old release cannot move
#     it backwards.
# Exit 0, `latest` written: it was missing or older, and it was re-read and resolves to --digest.
# Exit 1, nothing written:
#   - the release tag is missing, or does not resolve to --digest;
#   - a registry lookup, pull or inspect fails (a failure is never read as "missing" or "older");
#   - `latest` carries this very release ID under another digest;
#   - `latest` carries a calendar ID newer than this release that is not a release tag of this
#     repository (or is one with another digest): it is ahead of every real release, so it would
#     block every later move and needs a maintainer's attention;
#   - the image at --digest does not carry the version label <release-id>.
# Exit 1, after the write attempt: the copy failed, or `latest` cannot be read back or does not
# resolve to --digest.
#
# The tag is written by copying the manifest (an image or an image index, as published) by digest,
# so `latest` has exactly the digest of the release tag. That is why signatures and provenance,
# which are by digest, are unaffected by moving it.

set -euo pipefail

usage() {
  cat >&2 <<'EOF'
Usage: move-latest-tag.sh --registry <host/alias> --repository <name> --release-id <YYYYMMDD.n> \
         --digest <sha256:...>
EOF
  exit 2
}

registry=
repository=
release_id=
digest=

while (($# > 0)); do
  case $1 in
    --registry)
      registry=${2-}
      shift 2
      ;;
    --repository)
      repository=${2-}
      shift 2
      ;;
    --release-id)
      release_id=${2-}
      shift 2
      ;;
    --digest)
      digest=${2-}
      shift 2
      ;;
    *)
      usage
      ;;
  esac
done

[[ -n $registry && -n $repository && -n $release_id && -n $digest ]] || usage

# The index has no leading zeros, as in scripts/release-metadata.sh, and is capped so that the
# arithmetic below cannot overflow. Values from the registry are matched before any arithmetic.
calendar_re='^([0-9]{8})\.(0|[1-9][0-9]{0,5})$'
if ! [[ $release_id =~ $calendar_re ]]; then
  printf 'not a calendar release ID (YYYYMMDD.n): %s\n' "$release_id" >&2
  exit 2
fi
if ! [[ $digest =~ ^sha256:[0-9a-f]{64}$ ]]; then
  printf 'not a sha256 image digest: %s\n' "$digest" >&2
  exit 2
fi

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
image="$registry/$repository"
latest_tag=latest

# Prints the digest a tag resolves to. Returns 3 when the tag does not exist, and any other
# non-zero status for a registry error.
lookup_digest() {
  bash "$root/scripts/ecr-public-image-digest.sh" --repository "$repository" --tag "$1"
}

# Prints the version label of the image at a digest reference, empty when there is none.
#
# Every command is checked explicitly: errexit is not in effect inside a command substitution,
# and a failed pull must never be read as "no label" (which would count as older and be replaced).
image_version() {
  docker pull "$1" >/dev/null || return 1
  local version
  version=$(docker inspect "$1" --format '{{ with .Config.Labels }}{{ index . "org.opencontainers.image.version" }}{{ end }}') || return 1
  printf '%s\n' "$version"
}

# Sets id_date and id_seq (base 10 explicitly) from a calendar ID.
parse_calendar_id() {
  [[ $1 =~ $calendar_re ]] || return 1
  id_date=$((10#${BASH_REMATCH[1]}))
  id_seq=$((10#${BASH_REMATCH[2]}))
}

# Refuses an image whose version label is not this release; called just before the write.
check_candidate() {
  local version
  version=$(image_version "$image@$digest") || exit 1
  if [ "$version" != "$release_id" ]; then
    printf 'image %s@%s has version label "%s", not %s; refusing to move %s\n' \
      "$image" "$digest" "$version" "$release_id" "$latest_tag" >&2
    exit 1
  fi
}

# The release must already be published, consistently, under its release tag.
if release_digest=$(lookup_digest "$release_id"); then
  :
else
  status=$?
  if [ "$status" -eq 3 ]; then
    printf 'release tag %s:%s does not exist; nothing to promote\n' "$image" "$release_id" >&2
    exit 1
  fi
  exit "$status"
fi
if [ "$release_digest" != "$digest" ]; then
  printf 'release tag %s:%s is %s, not %s; refusing to move %s\n' \
    "$image" "$release_id" "$release_digest" "$digest" "$latest_tag" >&2
  exit 1
fi
parse_calendar_id "$release_id"
release_date=$id_date
release_seq=$id_seq

if current_digest=$(lookup_digest "$latest_tag"); then
  if [ "$current_digest" = "$digest" ]; then
    printf '%s:%s already is %s (release %s); nothing to do\n' "$image" "$latest_tag" "$digest" "$release_id"
    exit 0
  fi
  current_version=$(image_version "$image@$current_digest") || exit 1
  if parse_calendar_id "$current_version"; then
    current_date=$id_date
    current_seq=$id_seq
    if [ "$current_version" = "$release_id" ]; then
      printf '%s:%s is %s and carries release ID %s, but this release is %s; leaving it alone\n' \
        "$image" "$latest_tag" "$current_digest" "$current_version" "$digest" >&2
      exit 1
    fi
    if [ "$current_date" -gt "$release_date" ] || { [ "$current_date" -eq "$release_date" ] && [ "$current_seq" -gt "$release_seq" ]; }; then
      # Newer is normal when an older release's job is re-run, but only if it is a real release.
      if newer_digest=$(lookup_digest "$current_version"); then
        if [ "$newer_digest" = "$current_digest" ]; then
          printf '::notice::%s:%s is release %s, newer than %s; not moving it backwards\n' \
            "$image" "$latest_tag" "$current_version" "$release_id"
          exit 0
        fi
      else
        status=$?
        [ "$status" -eq 3 ] || exit "$status"
      fi
      printf '::warning::%s:%s (%s) is labelled %s, newer than %s, but %s is not a release tag of %s with that digest. It would block every later move; a maintainer must look at it (see RELEASING.md, Moving the latest tag).\n' \
        "$image" "$latest_tag" "$current_digest" "$current_version" "$release_id" "$current_version" "$image" >&2
      exit 1
    fi
    printf '%s:%s is release %s (%s); moving it forward to %s (%s)\n' \
      "$image" "$latest_tag" "$current_version" "$current_digest" "$release_id" "$digest"
  else
    printf '%s:%s (%s) has version label "%s", which is not a calendar release ID; treating it as older and replacing it with %s (%s)\n' \
      "$image" "$latest_tag" "$current_digest" "$current_version" "$release_id" "$digest"
  fi
else
  status=$?
  [ "$status" -eq 3 ] || exit "$status"
  printf '%s:%s does not exist yet; creating it for release %s (%s)\n' "$image" "$latest_tag" "$release_id" "$digest"
fi

check_candidate
docker buildx imagetools create --prefer-index=false --tag "$image:$latest_tag" "$image@$digest"

final_digest=$(lookup_digest "$latest_tag") || {
  printf 'could not read %s:%s back after the update\n' "$image" "$latest_tag" >&2
  exit 1
}
if [ "$final_digest" != "$digest" ]; then
  printf '%s:%s resolves to %s after the update, expected %s\n' "$image" "$latest_tag" "${final_digest:-<none>}" "$digest" >&2
  exit 1
fi
printf '%s:%s now is release %s (%s)\n' "$image" "$latest_tag" "$release_id" "$digest"
