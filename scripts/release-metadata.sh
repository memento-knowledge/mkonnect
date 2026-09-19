#!/usr/bin/env bash

set -euo pipefail

usage() {
  cat >&2 <<'EOF'
Usage: release-metadata.sh --repo <path> --tag <tag> --main-ref <ref> --output <path>
EOF
  exit 2
}

fail() {
  printf 'release metadata validation failed: %s\n' "$1" >&2
  exit 1
}

repo=
tag=
main_ref=
output=

while (($# > 0)); do
  case $1 in
    --repo)
      repo=${2-}
      shift 2
      ;;
    --tag)
      tag=${2-}
      shift 2
      ;;
    --main-ref)
      main_ref=${2-}
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

[[ -n $repo && -n $tag && -n $main_ref && -n $output ]] || usage
[[ -d $repo ]] || fail "repository does not exist: $repo"
[[ -f "$repo/charts/mkonnect/Chart.yaml" ]] || fail "Chart.yaml is missing"

object_type=$(git -C "$repo" cat-file -t "$tag" 2>/dev/null) || fail "tag does not exist: $tag"
[[ $object_type == tag ]] || fail "tag must be annotated: $tag"
[[ $tag =~ ^v(0|[1-9][0-9]*)[.](0|[1-9][0-9]*)[.](0|[1-9][0-9]*)$ ]] || fail "tag is not stable SemVer: $tag"

release_commit=$(git -C "$repo" rev-parse "$tag^{commit}") || fail "tag does not resolve to a commit: $tag"
git -C "$repo" rev-parse --verify "$main_ref^{commit}" >/dev/null 2>&1 || fail "main ref does not resolve: $main_ref"
git -C "$repo" merge-base --is-ancestor "$release_commit" "$main_ref" || fail "tag commit is not reachable from $main_ref"

chart="$repo/charts/mkonnect/Chart.yaml"
chart_version=$(awk '$1 == "version:" { print $2; exit }' "$chart")
app_version=$(awk '$1 == "appVersion:" { print $2; exit }' "$chart")
app_version=${app_version#\"}
app_version=${app_version%\"}
version=${tag#v}

[[ $chart_version == "$version" ]] || fail "chart version $chart_version does not match $version"
[[ $app_version == "$version" ]] || fail "chart appVersion $app_version does not match $version"

output_dir=$(dirname "$output")
[[ -d $output_dir ]] || fail "output directory does not exist: $output_dir"
output_tmp=$(mktemp "$output_dir/.release-metadata.XXXXXX")
trap 'rm -f "$output_tmp"' EXIT
printf 'version=%s\nrelease_commit=%s\n' "$version" "$release_commit" >"$output_tmp"
mv "$output_tmp" "$output"
