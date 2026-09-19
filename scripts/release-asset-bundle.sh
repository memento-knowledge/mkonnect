#!/usr/bin/env bash

set -euo pipefail

usage() {
  cat >&2 <<'EOF'
Usage: release-asset-bundle.sh --chart-dir <path> --source-package <path> --asset-dir <path> --output <path>
EOF
  exit 2
}

chart_dir=
source_package=
asset_dir=
output=

while (($# > 0)); do
  case $1 in
    --chart-dir)
      chart_dir=${2-}
      shift 2
      ;;
    --source-package)
      source_package=${2-}
      shift 2
      ;;
    --asset-dir)
      asset_dir=${2-}
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

[[ -n $chart_dir && -n $source_package && -n $asset_dir && -n $output ]] || usage
[[ -f $source_package ]] || {
  printf 'release source package does not exist: %s\n' "$source_package" >&2
  exit 1
}

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
verify_chart="$root/scripts/release-chart-source.sh"
package=$(basename "$source_package")
checksum="$package.sha256"
canonical_package="$asset_dir/$package"
canonical_checksum="$asset_dir/$checksum"

mkdir -p "$asset_dir"

if [[ ! -e $canonical_package && -e $canonical_checksum ]]; then
  printf 'release asset bundle has a checksum without its chart package\n' >&2
  exit 1
fi

if [[ -e $canonical_package ]]; then
  bash "$verify_chart" --chart-dir "$chart_dir" --package "$canonical_package"
  if [[ -e $canonical_checksum ]]; then
    (cd "$asset_dir" && sha256sum -c "$checksum")
    chart_upload=false
    checksum_upload=false
  else
    (cd "$asset_dir" && sha256sum "$package" >"$checksum")
    chart_upload=false
    checksum_upload=true
  fi
else
  bash "$verify_chart" --chart-dir "$chart_dir" --package "$source_package"
  cp "$source_package" "$canonical_package"
  (cd "$asset_dir" && sha256sum "$package" >"$checksum")
  chart_upload=true
  checksum_upload=true
fi

tmp_output=$(mktemp "${output}.tmp.XXXXXX")
trap 'rm -f "$tmp_output"' EXIT
printf 'chart_upload=%s\nchecksum_upload=%s\n' "$chart_upload" "$checksum_upload" >"$tmp_output"
mv "$tmp_output" "$output"
