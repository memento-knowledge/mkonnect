#!/usr/bin/env bash

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
validator="$root/scripts/release-metadata.sh"
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

test_count=0

new_repo() {
  local name=$1
  repo="$tmpdir/$name"
  git init -q -b main "$repo"
  git -C "$repo" config user.name "Release Metadata Test"
  git -C "$repo" config user.email "release-metadata-test@example.invalid"
  mkdir -p "$repo/charts/mkonnect"
  cat >"$repo/charts/mkonnect/Chart.yaml" <<'EOF'
apiVersion: v2
name: mkonnect
version: 1.2.3
appVersion: "20260919.0"
EOF
  git -C "$repo" add charts/mkonnect/Chart.yaml
  git -C "$repo" commit -qm "initial chart"
}

expect_success() {
  local name=$1
  local tag=$2
  local main_ref=$3
  local output="$tmpdir/$name.out"
  bash "$validator" --repo "$repo" --tag "$tag" --main-ref "$main_ref" --output "$output"
  test -s "$output"
  test_count=$((test_count + 1))
}

expect_failure() {
  local name=$1
  local tag=$2
  local main_ref=$3
  local output="$tmpdir/$name.out"
  if bash "$validator" --repo "$repo" --tag "$tag" --main-ref "$main_ref" --output "$output"; then
    printf 'expected %s to fail\n' "$name" >&2
    exit 1
  fi
  test_count=$((test_count + 1))
}

new_repo annotated-main
git -C "$repo" tag -a 20260919.0 -m "release 20260919.0"
expected_commit=$(git -C "$repo" rev-parse '20260919.0^{commit}')
expect_success annotated-main 20260919.0 main
grep -Fx "release_id=20260919.0" "$tmpdir/annotated-main.out"
grep -Fx "chart_version=1.2.3" "$tmpdir/annotated-main.out"
grep -Fx "release_commit=$expected_commit" "$tmpdir/annotated-main.out"

new_repo lightweight
git -C "$repo" tag 20260919.0
expect_failure lightweight 20260919.0 main

for invalid_tag in 20260919 20260919.a 20260919.00 20260229.0 20260230.0 20261301.0 20260001.0 v1.2.3; do
  new_repo "invalid-${invalid_tag//[^[:alnum:]]/-}"
  git -C "$repo" tag -a "$invalid_tag" -m "invalid release"
  expect_failure "invalid-$invalid_tag" "$invalid_tag" main
done

new_repo leap-day
sed -i.bak 's/appVersion: "20260919.0"/appVersion: "20240229.0"/' "$repo/charts/mkonnect/Chart.yaml"
rm "$repo/charts/mkonnect/Chart.yaml.bak"
git -C "$repo" add charts/mkonnect/Chart.yaml
git -C "$repo" commit -qm "prepare leap-day release"
git -C "$repo" tag -a 20240229.0 -m "release 20240229.0"
expect_success leap-day 20240229.0 main

new_repo missing-first-index
git -C "$repo" tag -a 20260919.1 -m "release 20260919.1"
expect_failure missing-first-index 20260919.1 main

new_repo missing-middle-index
git -C "$repo" tag -a 20260919.0 -m "release 20260919.0"
sed -i.bak 's/appVersion: "20260919.0"/appVersion: "20260919.2"/' "$repo/charts/mkonnect/Chart.yaml"
rm "$repo/charts/mkonnect/Chart.yaml.bak"
git -C "$repo" add charts/mkonnect/Chart.yaml
git -C "$repo" commit -qm "prepare third release"
git -C "$repo" tag -a 20260919.2 -m "release 20260919.2"
expect_failure missing-middle-index 20260919.2 main

new_repo oversized-index
sed -i.bak 's/appVersion: "20260919.0"/appVersion: "20260919.9223372036854775808"/' "$repo/charts/mkonnect/Chart.yaml"
rm "$repo/charts/mkonnect/Chart.yaml.bak"
git -C "$repo" add charts/mkonnect/Chart.yaml
git -C "$repo" commit -qm "prepare oversized release"
git -C "$repo" tag -a 20260919.9223372036854775808 -m "oversized release"
expect_failure oversized-index 20260919.9223372036854775808 main

new_repo contiguous-index
git -C "$repo" tag -a 20260919.0 -m "release 20260919.0"
sed -i.bak 's/appVersion: "20260919.0"/appVersion: "20260919.1"/' "$repo/charts/mkonnect/Chart.yaml"
rm "$repo/charts/mkonnect/Chart.yaml.bak"
git -C "$repo" add charts/mkonnect/Chart.yaml
git -C "$repo" commit -qm "prepare second release"
git -C "$repo" tag -a 20260919.1 -m "release 20260919.1"
expect_success contiguous-index 20260919.1 main

new_repo older-release-retry
git -C "$repo" tag -a 20260919.0 -m "release 20260919.0"
sed -i.bak 's/appVersion: "20260919.0"/appVersion: "20260919.1"/' "$repo/charts/mkonnect/Chart.yaml"
rm "$repo/charts/mkonnect/Chart.yaml.bak"
git -C "$repo" add charts/mkonnect/Chart.yaml
git -C "$repo" commit -qm "prepare later release"
git -C "$repo" tag -a 20260919.1 -m "release 20260919.1"
git -C "$repo" checkout -q --detach '20260919.0^{commit}'
expect_success older-release-retry 20260919.0 main

new_repo side-branch
git -C "$repo" switch -qc side
printf 'side branch\n' >"$repo/side"
git -C "$repo" add side
git -C "$repo" commit -qm "side change"
git -C "$repo" tag -a 20260919.0 -m "side release"
expect_failure side-branch 20260919.0 main

new_repo mismatched-chart
sed -i.bak 's/appVersion: "20260919.0"/appVersion: "20260919.1"/' "$repo/charts/mkonnect/Chart.yaml"
rm "$repo/charts/mkonnect/Chart.yaml.bak"
git -C "$repo" add charts/mkonnect/Chart.yaml
git -C "$repo" commit -qm "mismatch chart metadata"
git -C "$repo" tag -a 20260919.0 -m "mismatched release"
expect_failure mismatched-chart 20260919.0 main

printf 'release metadata tests passed: %d cases\n' "$test_count"
