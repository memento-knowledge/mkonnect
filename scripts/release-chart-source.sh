#!/usr/bin/env bash

set -euo pipefail

usage() {
  cat >&2 <<'EOF'
Usage: release-chart-source.sh --chart-dir <path> --package <path>
EOF
  exit 2
}

chart_dir=
package=

while (($# > 0)); do
  case $1 in
    --chart-dir)
      chart_dir=${2-}
      shift 2
      ;;
    --package)
      package=${2-}
      shift 2
      ;;
    *)
      usage
      ;;
  esac
done

[[ -n $chart_dir && -n $package ]] || usage
[[ -f "$chart_dir/Chart.yaml" ]] || {
  printf 'chart source is missing Chart.yaml: %s\n' "$chart_dir" >&2
  exit 1
}
[[ -f $package ]] || {
  printf 'chart package does not exist: %s\n' "$package" >&2
  exit 1
}

chart_name=$(awk '$1 == "name:" { print $2; exit }' "$chart_dir/Chart.yaml")
[[ -n $chart_name ]] || {
  printf 'chart source is missing a name\n' >&2
  exit 1
}

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT
tar -xzf "$package" -C "$tmpdir"
archived_chart="$tmpdir/$chart_name"
[[ -d $archived_chart ]] || {
  printf 'chart package does not contain %s\n' "$chart_name" >&2
  exit 1
}

source_metadata="$tmpdir/source-chart.yaml"
package_metadata="$tmpdir/package-chart.yaml"
helm show chart "$chart_dir" >"$source_metadata"
helm show chart "$package" >"$package_metadata"

if ! diff -u "$source_metadata" "$package_metadata"; then
  printf 'chart package metadata does not match the release source\n' >&2
  exit 1
fi

if ! diff -ruN --exclude Chart.yaml "$chart_dir" "$archived_chart"; then
  printf 'chart package content does not match the release source\n' >&2
  exit 1
fi
