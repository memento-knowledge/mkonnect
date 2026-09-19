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
appVersion: "1.2.3"
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
git -C "$repo" tag -a v1.2.3 -m "release 1.2.3"
expected_commit=$(git -C "$repo" rev-parse 'v1.2.3^{commit}')
expect_success annotated-main v1.2.3 main
grep -Fx "version=1.2.3" "$tmpdir/annotated-main.out"
grep -Fx "release_commit=$expected_commit" "$tmpdir/annotated-main.out"

new_repo lightweight
git -C "$repo" tag v1.2.3
expect_failure lightweight v1.2.3 main

for invalid_tag in v1.2 v1x2y3 v01.2.3 v1.02.3 v1.2.03; do
  new_repo "invalid-${invalid_tag//[^[:alnum:]]/-}"
  git -C "$repo" tag -a "$invalid_tag" -m "invalid release"
  expect_failure "invalid-$invalid_tag" "$invalid_tag" main
done

new_repo side-branch
git -C "$repo" switch -qc side
printf 'side branch\n' >"$repo/side"
git -C "$repo" add side
git -C "$repo" commit -qm "side change"
git -C "$repo" tag -a v1.2.3 -m "side release"
expect_failure side-branch v1.2.3 main

new_repo mismatched-chart
sed -i.bak 's/appVersion: "1.2.3"/appVersion: "1.2.4"/' "$repo/charts/mkonnect/Chart.yaml"
rm "$repo/charts/mkonnect/Chart.yaml.bak"
git -C "$repo" add charts/mkonnect/Chart.yaml
git -C "$repo" commit -qm "mismatch chart metadata"
git -C "$repo" tag -a v1.2.3 -m "mismatched release"
expect_failure mismatched-chart v1.2.3 main

printf 'release metadata tests passed: %d cases\n' "$test_count"
