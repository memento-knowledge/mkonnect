#!/usr/bin/env bash

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
bundle="$root/scripts/release-asset-bundle.sh"
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

cp -R "$root/charts/mkonnect" "$tmpdir/chart"
chart_name=$(awk '$1 == "name:" { print $2; exit }' "$tmpdir/chart/Chart.yaml")
chart_version=$(awk '$1 == "version:" { print $2; exit }' "$tmpdir/chart/Chart.yaml")
package="$chart_name-$chart_version.tgz"

mkdir "$tmpdir/first" "$tmpdir/retry" "$tmpdir/assets"
find "$tmpdir/chart" -type f -exec touch -t 202001010101 {} +
helm package "$tmpdir/chart" --destination "$tmpdir/first"
find "$tmpdir/chart" -type f -exec touch -t 202002020202 {} +
helm package "$tmpdir/chart" --destination "$tmpdir/retry"
test "$(sha256sum "$tmpdir/first/$package" | awk '{ print $1 }')" != \
  "$(sha256sum "$tmpdir/retry/$package" | awk '{ print $1 }')"

bash "$bundle" \
  --chart-dir "$tmpdir/chart" \
  --source-package "$tmpdir/first/$package" \
  --asset-dir "$tmpdir/assets" \
  --output "$tmpdir/first.env"
grep -qx 'chart_upload=true' "$tmpdir/first.env"
grep -qx 'checksum_upload=true' "$tmpdir/first.env"
first_digest=$(sha256sum "$tmpdir/assets/$package" | awk '{ print $1 }')

rm "$tmpdir/assets/$package.sha256"
bash "$bundle" \
  --chart-dir "$tmpdir/chart" \
  --source-package "$tmpdir/retry/$package" \
  --asset-dir "$tmpdir/assets" \
  --output "$tmpdir/retry.env"
grep -qx 'chart_upload=false' "$tmpdir/retry.env"
grep -qx 'checksum_upload=true' "$tmpdir/retry.env"
test "$first_digest" = "$(sha256sum "$tmpdir/assets/$package" | awk '{ print $1 }')"
(cd "$tmpdir/assets" && sha256sum -c "$package.sha256")

bash "$bundle" \
  --chart-dir "$tmpdir/chart" \
  --source-package "$tmpdir/retry/$package" \
  --asset-dir "$tmpdir/assets" \
  --output "$tmpdir/complete.env"
grep -qx 'chart_upload=false' "$tmpdir/complete.env"
grep -qx 'checksum_upload=false' "$tmpdir/complete.env"

rm "$tmpdir/assets/$package"
if bash "$bundle" \
  --chart-dir "$tmpdir/chart" \
  --source-package "$tmpdir/retry/$package" \
  --asset-dir "$tmpdir/assets" \
  --output "$tmpdir/checksum-only.env"; then
  printf 'checksum-only release assets unexpectedly reconciled\n' >&2
  exit 1
fi

printf 'release asset bundle tests passed\n'
