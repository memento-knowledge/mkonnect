#!/usr/bin/env bash
#
# Builds and publishes a release image to an Amazon ECR Public repository, or completes a
# partially published release, without ever overwriting an existing release tag.
#
# Two tags identify a release: the release ID (for example 20260919.0) and sha-<release commit>.
#   - Neither tag exists: build the image and push both tags. Nothing is pushed unless the build
#     succeeded. By default the release tag is pushed first and the commit tag second. With
#     --version-tag-last the order is reversed: the commit tag is pushed first and the release tag
#     last, so a push that fails part way through never leaves a release tag behind. The default
#     order is the one the API connector image has always used and must not change; only the
#     optional git-sync image, whose absence must be indistinguishable from "never published",
#     uses --version-tag-last. Either partial state is completed by a rerun, as below.
#   - Exactly one tag exists: its version and revision labels must match this release; the
#     missing tag is then created as an alias of the same digest, without rebuilding.
#   - Both tags exist: both must carry matching labels and the same digest.
# Anything else is a conflict and fails without changing the registry.
#
# On success the script writes `image=<reference>` and `image_digest=<digest>` to --output.

set -euo pipefail

usage() {
  cat >&2 <<'EOF'
Usage: reconcile-image-tags.sh --registry <host/alias> --repository <name> --dockerfile <path> \
         --release-id <id> --release-commit <sha> --output <path> [--context <dir>] [--version-tag-last]
EOF
  exit 2
}

registry=
repository=
dockerfile=
release_id=
release_commit=
output=
context=.
version_tag_last=false

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
    --dockerfile)
      dockerfile=${2-}
      shift 2
      ;;
    --release-id)
      release_id=${2-}
      shift 2
      ;;
    --release-commit)
      release_commit=${2-}
      shift 2
      ;;
    --output)
      output=${2-}
      shift 2
      ;;
    --context)
      context=${2-}
      shift 2
      ;;
    --version-tag-last)
      version_tag_last=true
      shift
      ;;
    *)
      usage
      ;;
  esac
done

[[ -n $registry && -n $repository && -n $dockerfile && -n $release_id && -n $release_commit && -n $output ]] || usage
[[ -f $dockerfile ]] || {
  printf 'Dockerfile does not exist: %s\n' "$dockerfile" >&2
  exit 1
}

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
image="$registry/$repository"
version_tag=$release_id
commit_tag="sha-$release_commit"

lookup_digest() {
  bash "$root/scripts/ecr-public-image-digest.sh" --repository "$repository" --tag "$1"
}

verify_labels() {
  local version revision
  docker pull "$image:$1" >/dev/null
  version=$(docker inspect "$image:$1" --format '{{ index .Config.Labels "org.opencontainers.image.version" }}')
  revision=$(docker inspect "$image:$1" --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}')
  if [ "$version" != "$release_id" ] || [ "$revision" != "$release_commit" ]; then
    printf 'existing image %s:%s does not match this release (version=%s revision=%s)\n' "$image" "$1" "$version" "$revision" >&2
    return 1
  fi
}

if version_digest=$(lookup_digest "$version_tag"); then
  version_exists=true
else
  status=$?
  [ "$status" -eq 3 ] || exit "$status"
  version_exists=false
  version_digest=
fi
if commit_digest=$(lookup_digest "$commit_tag"); then
  commit_exists=true
else
  status=$?
  [ "$status" -eq 3 ] || exit "$status"
  commit_exists=false
  commit_digest=
fi

if ! "$version_exists" && ! "$commit_exists"; then
  docker build \
    --file "$dockerfile" \
    --build-arg VERSION="$release_id" \
    --build-arg REVISION="$release_commit" \
    --tag "$image:$version_tag" \
    --tag "$image:$commit_tag" \
    "$context"
  if "$version_tag_last"; then
    docker push "$image:$commit_tag"
    docker push "$image:$version_tag"
  else
    docker push "$image:$version_tag"
    docker push "$image:$commit_tag"
  fi
elif "$version_exists" && ! "$commit_exists"; then
  verify_labels "$version_tag"
  docker buildx imagetools create --prefer-index=false --tag "$image:$commit_tag" "$image@$version_digest"
elif ! "$version_exists" && "$commit_exists"; then
  verify_labels "$commit_tag"
  docker buildx imagetools create --prefer-index=false --tag "$image:$version_tag" "$image@$commit_digest"
else
  verify_labels "$version_tag"
  verify_labels "$commit_tag"
fi

version_digest=$(lookup_digest "$version_tag")
commit_digest=$(lookup_digest "$commit_tag")
[ -n "$version_digest" ] && [ "$version_digest" = "$commit_digest" ]

output_dir=$(dirname "$output")
[[ -d $output_dir ]] || {
  printf 'output directory does not exist: %s\n' "$output_dir" >&2
  exit 1
}
output_tmp=$(mktemp "$output_dir/.reconcile-image-tags.XXXXXX")
trap 'rm -f "$output_tmp"' EXIT
printf 'image=%s\nimage_digest=%s\n' "$image" "$version_digest" >"$output_tmp"
mv "$output_tmp" "$output"
