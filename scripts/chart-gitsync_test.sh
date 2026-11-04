#!/usr/bin/env bash

# Verifies that the Helm chart renders the optional git-sync workload correctly:
# nothing renders while it is disabled (and the connector resources are unaffected by
# enabling it), required values and the replica-host format are validated, the image
# reference honours tag and digest, the pod is hardened (security context, read-only
# root filesystem, disk-backed /tmp, read-only Secret mounts), the registration token is
# a mounted file rather than an environment variable, the mirrors directory is explicit,
# the NetworkPolicy denies ingress, and no Service or container port exists.

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
chart="$root/charts/mkonnect"

connector_id=00000000-0000-0000-0000-000000000000
gitsync_id=11111111-1111-1111-1111-111111111111

common=(--set image.repository=example/repo
  --set config.gatewayUrl=wss://example.invalid/ws
  --set "config.connectorId=$connector_id")

enabled=("${common[@]}"
  --set gitSync.enabled=true
  --set gitSync.image.repository=example/gitsync
  --set "gitSync.config.connectorId=$gitsync_id"
  --set gitSync.config.replicaHost=replica.example.invalid)

fail() {
  printf 'chart git-sync test failed: %s\n' "$1" >&2
  exit 1
}

# render <helm args...> prints the rendered manifests; the caller supplies the value sets.
render() { helm template t "$chart" "$@"; }

# deployment <helm args...> prints only the git-sync Deployment.
deployment() { helm template t "$chart" "$@" --show-only templates/gitsync-deployment.yaml; }

# expect_failure <expected message fragment> <helm args...> requires the render to fail with
# a message containing the fragment, so a regression that fails for another reason is caught.
expect_failure() {
  local want=$1 err
  shift
  if err=$(helm template t "$chart" "$@" 2>&1 >/dev/null); then
    fail "render succeeded but should have failed with: $want"
  fi
  grep -qF -- "$want" <<<"$err" || fail "render failed without the expected message '$want': $err"
}

# 1. Disabled (the default): no git-sync resource of any kind renders.
out=$(render "${common[@]}")
if grep -qi 'gitsync' <<<"$out"; then
  fail "git-sync artifacts rendered while gitSync.enabled is false"
fi
out=$(render "${common[@]}" --set gitSync.enabled=false \
  --set gitSync.image.repository=example/gitsync --set gitSync.networkPolicy.enabled=true)
if grep -qi 'gitsync' <<<"$out"; then
  fail "git-sync artifacts rendered with gitSync.enabled=false and other gitSync values set"
fi

# 2. Enabling git-sync must not change any connector resource.
for tpl in deployment secret pvc serviceaccount; do
  before=$(helm template t "$chart" "${common[@]}" --show-only "templates/$tpl.yaml")
  after=$(helm template t "$chart" "${enabled[@]}" --show-only "templates/$tpl.yaml")
  [ "$before" = "$after" ] || fail "enabling git-sync changed the connector's $tpl.yaml"
done

# 3. Minimal enabled render: the expected resources, and only those.
out=$(render "${enabled[@]}")
for kind in Deployment PersistentVolumeClaim NetworkPolicy ServiceAccount; do
  grep -q "^kind: $kind\$" <<<"$out" || fail "no $kind rendered for git-sync"
done
grep -q 'name: t-mkonnect-gitsync$' <<<"$out" || fail "git-sync Deployment is not named <fullname>-gitsync"
grep -q 'claimName: t-mkonnect-gitsync-data' <<<"$out" || fail "git-sync does not use its own PVC"
if grep -q 'claimName: t-mkonnect-data' <<<"$(deployment "${enabled[@]}")"; then
  fail "git-sync mounts the connector's PVC"
fi
dep=$(deployment "${enabled[@]}")
grep -q 'replicas: 1$' <<<"$dep" || fail "git-sync replicas is not 1"
grep -q 'type: Recreate' <<<"$dep" || fail "git-sync strategy is not Recreate"
grep -q 'image: "example/gitsync:20261022.0"' <<<"$dep" || fail "image tag does not default to Chart.appVersion"
grep -q -- '- /mkonnect-gitsync' <<<"$dep" || fail "startup probe does not run /mkonnect-gitsync"
grep -q -- '- --health' <<<"$dep" || fail "startup probe does not pass --health"
grep -q 'storage: 20Gi' <<<"$out" || fail "git-sync PVC size does not default to 20Gi"
grep -q 'value: "wss://example.invalid/ws"' <<<"$dep" || fail "gateway URL does not default to config.gatewayUrl"
grep -q 'name: CONNECTOR_ID' <<<"$dep" || fail "CONNECTOR_ID not rendered"
grep -q "value: \"$gitsync_id\"" <<<"$dep" || fail "git-sync connector ID not rendered"
if grep -q "$connector_id" <<<"$dep"; then
  fail "git-sync Deployment carries the connector's ID"
fi
grep -q 'cpu: 250m' <<<"$dep" || fail "default CPU request not rendered"
grep -q 'memory: 512Mi' <<<"$dep" || fail "default memory request not rendered"
grep -q 'memory: 2Gi' <<<"$dep" || fail "default memory limit not rendered"

# 4. Required values fail with a clear message.
expect_failure "gitSync.image.repository is required" \
  --set image.repository=example/repo --set config.gatewayUrl=wss://example.invalid/ws \
  --set "config.connectorId=$connector_id" --set gitSync.enabled=true \
  --set "gitSync.config.connectorId=$gitsync_id" --set gitSync.config.replicaHost=replica.example.invalid
expect_failure "gitSync.config.connectorId is required" \
  "${common[@]}" --set gitSync.enabled=true --set gitSync.image.repository=example/gitsync \
  --set gitSync.config.replicaHost=replica.example.invalid
expect_failure "gitSync.config.replicaHost is required" \
  "${common[@]}" --set gitSync.enabled=true --set gitSync.image.repository=example/gitsync \
  --set "gitSync.config.connectorId=$gitsync_id"
expect_failure "must differ from config.connectorId" \
  "${enabled[@]}" --set "gitSync.config.connectorId=$connector_id"

# 5. replicaHost accepts host and host:port, and rejects a scheme, a path and other shapes.
for good in replica.example.invalid replica.example.invalid:8443 replica; do
  deployment "${enabled[@]}" --set "gitSync.config.replicaHost=$good" >/dev/null ||
    fail "valid replicaHost '$good' was rejected"
done
for bad in https://replica.example.invalid replica.example.invalid/path replica.example.invalid/ \
  user@replica.example.invalid 'replica example' replica.example.invalid: replica.example.invalid:70000 \
  replica.example.invalid:0 -replica.example.invalid; do
  expect_failure "gitSync.config.replicaHost" "${enabled[@]}" --set "gitSync.config.replicaHost=$bad"
done
dep=$(deployment "${enabled[@]}" --set gitSync.config.replicaHost=replica.example.invalid:8443)
grep -q 'value: "replica.example.invalid:8443"' <<<"$dep" || fail "replicaHost with a port not rendered verbatim"
grep -q 'name: GIT_SYNC_REPLICA_HOST' <<<"$dep" || fail "GIT_SYNC_REPLICA_HOST not rendered"

# 6. Image reference: a digest wins over the tag; a tag overrides the appVersion default.
digest=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
dep=$(deployment "${enabled[@]}" --set gitSync.image.tag=20260101.0 --set "gitSync.image.digest=$digest")
grep -q "image: \"example/gitsync@$digest\"" <<<"$dep" || fail "digest does not win over tag"
if grep -q '20260101.0' <<<"$dep"; then fail "tag leaked into a digest-pinned image reference"; fi
dep=$(deployment "${enabled[@]}" --set gitSync.image.tag=20260101.0)
grep -q 'image: "example/gitsync:20260101.0"' <<<"$dep" || fail "explicit image tag not used"
expect_failure "gitSync.image.digest must look like" "${enabled[@]}" --set gitSync.image.digest=latest

# 7. Security context.
dep=$(deployment "${enabled[@]}")
for want in 'runAsNonRoot: true' 'runAsUser: 65532' 'runAsGroup: 65532' 'fsGroup: 65532' \
  'readOnlyRootFilesystem: true' 'allowPrivilegeEscalation: false' 'type: RuntimeDefault' \
  'automountServiceAccountToken: false'; do
  grep -q "$want" <<<"$dep" || fail "security setting missing: $want"
done
# capabilities.drop must be exactly [ALL].
caps=$(awk '/capabilities:/{f=1;next} f&&/drop:/{d=1;next} d&&/- /{print $2;next} d{exit}' <<<"$dep")
[ "$caps" = "ALL" ] || fail "capabilities.drop is not [ALL] (got: $caps)"
if grep -q 'privileged: true' <<<"$dep"; then fail "privileged container"; fi
sa=$(render "${enabled[@]}" --show-only templates/gitsync-serviceaccount.yaml)
grep -q 'automountServiceAccountToken: false' <<<"$sa" || fail "ServiceAccount token is automounted"

# 8. Disk-backed /tmp with a size limit, TMPDIR pointing at it, never memory-backed.
grep -q 'mountPath: /tmp' <<<"$dep" || fail "/tmp not mounted"
grep -q 'name: TMPDIR' <<<"$dep" || fail "TMPDIR not set"
tmpvol=$(awk '/^        - name: tmp$/{f=1} f{print} f&&/sizeLimit/{exit}' <<<"$dep")
grep -q 'emptyDir:' <<<"$tmpvol" || fail "/tmp is not an emptyDir"
grep -q 'sizeLimit: "5Gi"' <<<"$tmpvol" || fail "/tmp sizeLimit does not default to 5Gi"
if grep -q 'medium' <<<"$dep"; then fail "an emptyDir is memory-backed"; fi
dep2=$(deployment "${enabled[@]}" --set gitSync.tmp.sizeLimit=40Gi)
grep -q 'sizeLimit: "40Gi"' <<<"$dep2" || fail "tmp.sizeLimit override not rendered"

# 9. Explicit paths: key, mirrors, and the default provider config on the data volume.
grep -q 'value: "/data/gitsync-key"' <<<"$dep" || fail "KEY_FILE not /data/gitsync-key"
grep -q 'name: GIT_SYNC_MIRRORS_DIR' <<<"$dep" || fail "GIT_SYNC_MIRRORS_DIR not set"
grep -q 'value: "/data/mirrors"' <<<"$dep" || fail "mirrors dir is not /data/mirrors"
grep -q 'value: "/data/git-sync.json"' <<<"$dep" || fail "GIT_SYNC_CONFIG_FILE does not default to /data/git-sync.json"
if grep -q 'name: provider-config' <<<"$dep"; then fail "provider-config mount rendered without a Secret"; fi
expect_failure "gitSync.config.keyFile must be a path under /data" "${enabled[@]}" --set gitSync.config.keyFile=/etc/key

# 10. Provider config Secret: mounted read-only, 0440, outside /data; mirrors stay on /data.
dep=$(deployment "${enabled[@]}" --set gitSync.providerConfig.secretName=my-providers)
grep -q 'secretName: "my-providers"' <<<"$dep" || fail "provider-config Secret not mounted"
grep -q 'mountPath: /etc/mkonnect-gitsync-providers' <<<"$dep" || fail "provider-config mountPath wrong"
grep -q 'value: "/etc/mkonnect-gitsync-providers/git-sync.json"' <<<"$dep" || fail "GIT_SYNC_CONFIG_FILE does not point at the mounted Secret"
grep -q 'value: "/data/mirrors"' <<<"$dep" || fail "mirrors dir moved when a provider Secret is used"
vol=$(awk '/^        - name: provider-config$/{f=1} f{print} f&&/path:/{exit}' <<<"$dep")
grep -q 'defaultMode: 0440' <<<"$vol" || fail "provider-config Secret defaultMode is not 0440"
mnt=$(awk '/^            - name: provider-config$/{f=1} f{print} f&&/readOnly/{exit}' <<<"$dep")
grep -q 'readOnly: true' <<<"$mnt" || fail "provider-config mount is not read-only"

# 11. Registration token: a read-only mounted file, never an environment variable.
dep=$(deployment "${enabled[@]}")
if grep -qE 'name: REGISTRATION_TOKEN$' <<<"$dep"; then fail "REGISTRATION_TOKEN env var rendered"; fi
if grep -q 'REGISTRATION_TOKEN_FILE' <<<"$dep"; then fail "token file env rendered with no token source"; fi
if grep -q 'gitsync-registration' <<<"$(render "${enabled[@]}")"; then fail "token Secret rendered with no token supplied"; fi
dep=$(deployment "${enabled[@]}" --set gitSync.config.registrationToken=s3cret-value)
if grep -qE 'name: REGISTRATION_TOKEN$' <<<"$dep"; then fail "REGISTRATION_TOKEN env var rendered"; fi
if grep -q 's3cret-value' <<<"$dep"; then fail "token value leaked into the Deployment"; fi
grep -q 'name: REGISTRATION_TOKEN_FILE' <<<"$dep" || fail "REGISTRATION_TOKEN_FILE not set"
grep -q 'value: "/etc/mkonnect-gitsync/registration-token/registration-token"' <<<"$dep" || fail "token file path wrong"
vol=$(awk '/^        - name: registration-token$/{f=1} f{print} f&&/path:/{exit}' <<<"$dep")
grep -q 'secretName: "t-mkonnect-gitsync-registration"' <<<"$vol" || fail "chart-managed token Secret not mounted"
grep -q 'defaultMode: 0440' <<<"$vol" || fail "token Secret defaultMode is not 0440"
mnt=$(awk '/^            - name: registration-token$/{f=1} f{print} f&&/readOnly/{exit}' <<<"$dep")
grep -q 'readOnly: true' <<<"$mnt" || fail "token mount is not read-only"
sec=$(render "${enabled[@]}" --set gitSync.config.registrationToken=s3cret-value --show-only templates/gitsync-secret.yaml)
grep -q "registration-token: \"$(printf %s s3cret-value | base64)\"" <<<"$sec" || fail "chart-managed token Secret content wrong"
# An existing Secret wins over a value and no chart-managed Secret is created.
out=$(render "${enabled[@]}" --set gitSync.config.registrationToken=s3cret-value \
  --set gitSync.config.registrationTokenSecret.name=my-token --set gitSync.config.registrationTokenSecret.key=tok)
grep -q 'secretName: "my-token"' <<<"$out" || fail "referenced token Secret not mounted"
vol=$(awk '/^        - name: registration-token$/{f=1} f{print} f&&/path:/{exit}' <<<"$out")
grep -q 'defaultMode: 0440' <<<"$vol" || fail "referenced token Secret defaultMode is not 0440"
grep -q 'value: "/etc/mkonnect-gitsync/registration-token/tok"' <<<"$out" || fail "token file path ignores registrationTokenSecret.key"
if grep -q 't-mkonnect-gitsync-registration' <<<"$out"; then fail "chart-managed token Secret created alongside a referenced one"; fi

# 12. NetworkPolicy: ingress denied for the git-sync pods; egress untouched by default.
np=$(render "${enabled[@]}" --show-only templates/gitsync-networkpolicy.yaml)
grep -q 'app.kubernetes.io/name: mkonnect-gitsync' <<<"$np" || fail "NetworkPolicy does not select the git-sync pods"
grep -q -- '- Ingress' <<<"$np" || fail "NetworkPolicy lacks the Ingress policyType"
if grep -q -- '- Egress' <<<"$np"; then fail "egress restricted by default"; fi
if grep -qE '^  (ingress|egress):' <<<"$np"; then fail "NetworkPolicy has ingress/egress rules by default"; fi
out=$(render "${enabled[@]}" --set gitSync.networkPolicy.enabled=false)
if grep -q 'kind: NetworkPolicy' <<<"$out"; then fail "NetworkPolicy rendered while disabled"; fi
# The git-sync and connector pods must not select one another.
if grep -q 'app.kubernetes.io/name: mkonnect$' <<<"$(deployment "${enabled[@]}")"; then
  fail "git-sync pods carry the connector's selector label"
fi

# 13. Optional egress allow-list: CIDRs on ports, plus DNS.
np=$(render "${enabled[@]}" --set 'gitSync.networkPolicy.egress.cidrs={203.0.113.0/24,198.51.100.7/32}' \
  --show-only templates/gitsync-networkpolicy.yaml)
grep -q -- '- Egress' <<<"$np" || fail "Egress policyType missing when cidrs set"
grep -q 'cidr: "203.0.113.0/24"' <<<"$np" || fail "first egress CIDR missing"
grep -q 'cidr: "198.51.100.7/32"' <<<"$np" || fail "second egress CIDR missing"
grep -q 'port: 443' <<<"$np" || fail "egress does not default to port 443"
grep -q 'port: 53' <<<"$np" || fail "DNS egress missing"
grep -q 'protocol: UDP' <<<"$np" || fail "DNS over UDP missing"
grep -q 'kubernetes.io/metadata.name: "kube-system"' <<<"$np" || fail "DNS namespace selector missing"
np=$(render "${enabled[@]}" --set 'gitSync.networkPolicy.egress.cidrs={203.0.113.0/24}' \
  --set 'gitSync.networkPolicy.egress.ports={443,8443}' --show-only templates/gitsync-networkpolicy.yaml)
grep -q 'port: 8443' <<<"$np" || fail "custom egress port missing"

# 14. No Service and no container port: git-sync accepts no inbound connections.
out=$(render "${enabled[@]}" --set 'gitSync.networkPolicy.egress.cidrs={203.0.113.0/24}')
if grep -q '^kind: Service$' <<<"$out"; then fail "a Service is rendered"; fi
if grep -qE 'containerPort|hostPort|hostNetwork' <<<"$(deployment "${enabled[@]}")"; then
  fail "git-sync Deployment exposes a port"
fi

# 15. Custom CA bundle for git: GIT_SSL_CAINFO, 0444 read-only mount, no overlap with other mounts.
if grep -q 'GIT_SSL_CAINFO' <<<"$(deployment "${enabled[@]}")"; then fail "GIT_SSL_CAINFO set without caBundle"; fi
dep=$(deployment "${enabled[@]}" --set gitSync.caBundle.secretName=my-ca)
grep -q 'name: GIT_SSL_CAINFO' <<<"$dep" || fail "GIT_SSL_CAINFO not rendered"
grep -q 'value: "/etc/mkonnect-gitsync-ca/ca-bundle.pem"' <<<"$dep" || fail "GIT_SSL_CAINFO value wrong"
grep -q 'secretName: "my-ca"' <<<"$dep" || fail "CA Secret not mounted"
grep -q 'defaultMode: 0444' <<<"$dep" || fail "CA Secret defaultMode is not 0444"
mnt=$(awk '/^            - name: ca-bundle$/{f=1} f{print} f&&/readOnly/{exit}' <<<"$dep")
grep -q 'readOnly: true' <<<"$mnt" || fail "CA mount is not read-only"
for bad in /data /data/ca /tmp /tmp/ca / /etc /etc/mkonnect-gitsync /etc/mkonnect-gitsync/registration-token \
  /etc/mkonnect-gitsync/registration-token/ca /etc/mkonnect-gitsync-providers /etc/mkonnect-gitsync-providers/ca \
  relative/path; do
  expect_failure "gitSync.caBundle.mountPath" "${enabled[@]}" \
    --set gitSync.caBundle.secretName=my-ca --set "gitSync.caBundle.mountPath=$bad"
done
# A sibling of another mount that merely shares a name prefix is fine.
deployment "${enabled[@]}" --set gitSync.caBundle.secretName=my-ca \
  --set gitSync.caBundle.mountPath=/etc/mkonnect-gitsync-ca2 >/dev/null ||
  fail "a non-overlapping CA mountPath was rejected"

# 16. All mounts together render without overlap and keep the connector's own mounts apart.
out=$(render "${enabled[@]}" --set gitSync.caBundle.secretName=my-ca \
  --set gitSync.providerConfig.secretName=my-providers \
  --set gitSync.config.registrationTokenSecret.name=my-token \
  --set caBundle.secretName=conn-ca --set credentials.secretName=conn-creds)
grep -q 'name: CA_BUNDLE' <<<"$out" || fail "connector CA_BUNDLE lost when git-sync is enabled"

# 17. Naming overrides and the dedicated ServiceAccount.
out=$(helm template rel "$chart" "${enabled[@]}" --set fullnameOverride=custom --show-only templates/gitsync-deployment.yaml)
grep -q 'name: custom-gitsync$' <<<"$out" || fail "fullnameOverride not honoured"
out=$(render "${enabled[@]}" --set gitSync.serviceAccount.create=false --show-only templates/gitsync-deployment.yaml)
grep -q 'serviceAccountName: default' <<<"$out" || fail "serviceAccount.create=false did not fall back to default"
out=$(render "${enabled[@]}" --set gitSync.serviceAccount.create=false)
if grep -q 'name: t-mkonnect-gitsync$' <<<"$(grep -A3 '^kind: ServiceAccount' <<<"$out")"; then
  fail "git-sync ServiceAccount rendered with create=false"
fi

# 18. NOTES mention git-sync only when enabled.
notes=$(helm install t "$chart" "${enabled[@]}" --dry-run=client 2>&1) || fail "dry-run install failed: $notes"
grep -q 'git-sync is enabled' <<<"$notes" || fail "NOTES lack the git-sync section when enabled"
notes=$(helm install t "$chart" "${common[@]}" --dry-run=client 2>&1) || fail "dry-run install failed: $notes"
if grep -qi 'git-sync' <<<"$notes"; then fail "NOTES mention git-sync while disabled"; fi

printf 'chart git-sync tests passed\n'
