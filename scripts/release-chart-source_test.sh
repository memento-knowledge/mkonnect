#!/usr/bin/env bash

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
verify="$root/scripts/release-chart-source.sh"
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

cp -R "$root/charts/mkonnect" "$tmpdir/chart"
helm package "$tmpdir/chart" --destination "$tmpdir"
package="$tmpdir/mkonnect-0.1.0.tgz"
bash "$verify" --chart-dir "$tmpdir/chart" --package "$package"

sleep 1
mkdir "$tmpdir/retry"
helm package "$root/charts/mkonnect" --destination "$tmpdir/retry"
retry_package="$tmpdir/retry/mkonnect-0.1.0.tgz"
bash "$verify" --chart-dir "$tmpdir/chart" --package "$retry_package"

sed -i.bak 's/Memento on-prem connector/Changed connector/' "$tmpdir/chart/Chart.yaml"
rm "$tmpdir/chart/Chart.yaml.bak"
if bash "$verify" --chart-dir "$tmpdir/chart" --package "$package"; then
  printf 'stale chart package unexpectedly matched changed source\n' >&2
  exit 1
fi

printf 'release chart source tests passed\n'
