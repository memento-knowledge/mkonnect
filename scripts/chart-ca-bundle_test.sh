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
# The default mountPath must not nest under the credentials mount. The rendered form is
# quoted ("/etc/mkonnect-ca"), so match the quoted prefix; "/etc/mkonnect-ca" must not be
# treated as nested under "/etc/mkonnect".
if grep -qE 'mountPath: "/etc/mkonnect(/|")' <<<"$out"; then
  fail "ca-bundle mount nested inside or equal to the credentials mount"
fi

# A mountPath that would collide with the credentials mount must be rejected by the
# chart (template fail), not rendered into a broken pod.
if helm template t "$chart" "${common[@]}" \
  --set caBundle.secretName=my-ca --set caBundle.mountPath=/etc/mkonnect/ca >/dev/null 2>&1; then
  fail "chart rendered a CA mountPath nested under /etc/mkonnect instead of failing"
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
