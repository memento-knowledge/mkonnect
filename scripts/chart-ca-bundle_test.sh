#!/usr/bin/env bash

# Verifies that the Helm chart renders the custom CA-bundle Secret mount correctly:
# the volume, the read-only volumeMount at a path that does not nest inside the
# credentials mount, and the CA_BUNDLE env var — and that none of these appear when
# no CA bundle is configured.

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
chart="$root/charts/mkonnect"

common=(--set image.repository=example/repo
  --set config.gatewayUrl=wss://example.invalid/ws
  --set config.connectorId=00000000-0000-0000-0000-000000000000)

fail() {
  printf 'chart CA-bundle test failed: %s\n' "$1" >&2
  exit 1
}

# 1. CA bundle set, credentials unset: the mount must render and must sit at a sibling
#    path, never nested inside the read-only /etc/mkonnect credentials mount.
out=$(helm template t "$chart" "${common[@]}" --set caBundle.secretName=my-ca)
grep -q 'name: CA_BUNDLE' <<<"$out" || fail "CA_BUNDLE env not rendered"
grep -q 'value: "/etc/mkonnect-ca/ca-bundle.pem"' <<<"$out" || fail "CA_BUNDLE value wrong"
grep -q 'mountPath: "/etc/mkonnect-ca"' <<<"$out" || fail "ca-bundle mountPath not rendered"
grep -q 'defaultMode: 0444' <<<"$out" || fail "ca-bundle defaultMode not 0444"
grep -q 'secretName: "my-ca"' <<<"$out" || fail "ca-bundle secret volume not rendered"
if grep -q 'mountPath: /etc/mkonnect/ca' <<<"$out"; then
  fail "ca-bundle mount nested inside the credentials mount"
fi

# 2. CA bundle set together with credentials: both mounts render independently.
out=$(helm template t "$chart" "${common[@]}" \
  --set caBundle.secretName=my-ca --set credentials.secretName=my-creds)
grep -q 'name: ca-bundle' <<<"$out" || fail "ca-bundle volume missing when creds also set"
grep -q 'name: credentials' <<<"$out" || fail "credentials volume missing when CA also set"

# 3. No CA bundle configured: nothing CA-related renders.
out=$(helm template t "$chart" "${common[@]}")
if grep -qE 'CA_BUNDLE|ca-bundle' <<<"$out"; then
  fail "CA-bundle artifacts rendered without caBundle.secretName"
fi

printf 'chart CA-bundle tests passed\n'
