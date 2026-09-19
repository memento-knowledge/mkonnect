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
[[ $tag =~ ^([0-9]{4})([0-9]{2})([0-9]{2})[.]([0-9]+)$ ]] || fail "tag is not YYYYMMDD.n: $tag"

year=${BASH_REMATCH[1]}
month=${BASH_REMATCH[2]}
day=${BASH_REMATCH[3]}
release_index=${BASH_REMATCH[4]}
[[ $release_index == 0 || $release_index =~ ^[1-9][0-9]*$ ]] || fail "release index must be zero-based without leading zeros: $tag"

year_number=$((10#$year))
month_number=$((10#$month))
day_number=$((10#$day))
((year_number >= 1 && month_number >= 1 && month_number <= 12)) || fail "tag date is invalid: $tag"
case $month_number in
  1 | 3 | 5 | 7 | 8 | 10 | 12) days_in_month=31 ;;
  4 | 6 | 9 | 11) days_in_month=30 ;;
  2)
    if ((year_number % 400 == 0 || (year_number % 4 == 0 && year_number % 100 != 0))); then
      days_in_month=29
    else
      days_in_month=28
    fi
    ;;
esac
((day_number >= 1 && day_number <= days_in_month)) || fail "tag date is invalid: $tag"

release_day="$year$month$day"
release_number=$((10#$release_index))
while IFS= read -r existing_tag; do
  [[ $existing_tag =~ ^${release_day}[.]([0-9]+)$ ]] || continue
  existing_index=${BASH_REMATCH[1]}
  [[ $existing_index == 0 || $existing_index =~ ^[1-9][0-9]*$ ]] || continue
  existing_number=$((10#$existing_index))
  ((existing_number <= release_number)) || fail "release index $existing_index already exists after $tag"
done < <(git -C "$repo" for-each-ref --format='%(refname:strip=2)' "refs/tags/$release_day.*")

for ((index = 0; index <= release_number; index++)); do
  expected_tag="$release_day.$index"
  git -C "$repo" show-ref --verify --quiet "refs/tags/$expected_tag" || fail "release index $index is missing before $tag"
  [[ $(git -C "$repo" cat-file -t "$expected_tag" 2>/dev/null) == tag ]] || fail "release tag must be annotated: $expected_tag"
done

release_commit=$(git -C "$repo" rev-parse "$tag^{commit}") || fail "tag does not resolve to a commit: $tag"
git -C "$repo" rev-parse --verify "$main_ref^{commit}" >/dev/null 2>&1 || fail "main ref does not resolve: $main_ref"
git -C "$repo" merge-base --is-ancestor "$release_commit" "$main_ref" || fail "tag commit is not reachable from $main_ref"

chart="$repo/charts/mkonnect/Chart.yaml"
chart_version=$(awk '$1 == "version:" { print $2; exit }' "$chart")
app_version=$(awk '$1 == "appVersion:" { print $2; exit }' "$chart")
app_version=${app_version#\"}
app_version=${app_version%\"}

[[ -n $chart_version ]] || fail "chart version is missing"
[[ $app_version == "$tag" ]] || fail "chart appVersion $app_version does not match release ID $tag"

output_dir=$(dirname "$output")
[[ -d $output_dir ]] || fail "output directory does not exist: $output_dir"
output_tmp=$(mktemp "$output_dir/.release-metadata.XXXXXX")
trap 'rm -f "$output_tmp"' EXIT
printf 'release_id=%s\nchart_version=%s\nrelease_commit=%s\n' "$tag" "$chart_version" "$release_commit" >"$output_tmp"
mv "$output_tmp" "$output"
