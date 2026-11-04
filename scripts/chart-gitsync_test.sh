#!/usr/bin/env bash

# Verifies that the Helm chart renders the optional git-sync workload correctly:
# nothing renders while it is disabled (and the connector resources are unaffected by
# enabling it), the templates also render for a release whose stored values predate
# git-sync (Helm 3 `--reuse-values`), required values and the replica-host format are
# validated, the image reference honours tag and digest, the pod is hardened, the
# registration token is a mounted file rather than an environment variable, the mirrors
# directory is explicit, the NetworkPolicy denies ingress, and no Service or container
# port exists.
#
# Assertions are made on individual rendered objects (selected by kind and name) and on
# indentation-delimited sections of them, not on loose matches against the whole output,
# so a setting moved to the wrong object or the wrong level fails the test.
#
# It uses only `helm template`, which needs no cluster.

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
chart="$root/charts/mkonnect"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

appver=$(sed -n 's/^appVersion: "\(.*\)"$/\1/p' "$chart/Chart.yaml")
[ -n "$appver" ] || {
  printf 'chart git-sync test failed: cannot read appVersion from Chart.yaml\n' >&2
  exit 1
}

connector_id=00000000-0000-0000-0000-000000000000
gitsync_id=11111111-1111-1111-1111-111111111111
dep_name=t-mkonnect-gitsync

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

# render <chart dir> <helm args...> prints every rendered manifest.
render_in() {
  local dir=$1
  shift
  helm template t "$dir" "$@"
}
render() { render_in "$chart" "$@"; }

# doc <Kind> <name> reads rendered manifests on stdin and prints the one object with that
# kind and metadata.name (nothing if there is none).
doc() {
  awk -v kind="$1" -v name="$2" '
    function flush() { if (buf != "" && hasKind && hasName) printf "%s", buf; buf = ""; hasKind = 0; hasName = 0 }
    /^---$/ { flush(); next }
    { buf = buf $0 "\n"; if ($0 == "kind: " kind) hasKind = 1; if ($0 == "  name: " name) hasName = 1 }
    END { flush() }'
}

# section "<exact line>" reads text on stdin and prints that line and the more deeply
# indented lines that follow it (a YAML mapping or list item), stopping at the next line
# with the same or lower indentation.
section() {
  awk -v want="$1" '
    function indent(s) { match(s, /^ */); return RLENGTH }
    found && $0 !~ /^[[:space:]]*$/ && indent($0) <= base { exit }
    found { print; next }
    $0 == want { found = 1; base = indent($0); print }'
}

# The Deployment, with the sections the assertions look at.
deployment() { render "$@" | doc Deployment "$dep_name"; }
pod_security() { section '      securityContext:'; }
container_security() { section '          securityContext:'; }
volume() { section "        - name: $1"; }
mount() { section "            - name: $1"; }
env_var() { section "            - name: $1"; }

# check <text> <ERE> <message> requires the text to match; refute requires it not to.
check() { grep -Eq -- "$2" <<<"$1" || fail "$3"; }
refute() { if grep -Eq -- "$2" <<<"$1"; then fail "$3"; fi; }

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

# overlaps <a> <b> succeeds when two absolute paths are equal or one is nested in the other.
overlaps() {
  local a=${1%/} b=${2%/}
  [ "$a" = "$b" ] || [[ $b == "$a"/* ]] || [[ $a == "$b"/* ]]
}

# 1. Disabled (the default): no git-sync resource of any kind renders.
out=$(render "${common[@]}")
refute "$out" -i 'gitsync' "git-sync artifacts rendered while gitSync.enabled is false"
out=$(render "${common[@]}" --set gitSync.enabled=false \
  --set gitSync.image.repository=example/gitsync --set gitSync.networkPolicy.enabled=true)
refute "$out" -i 'gitsync' "git-sync artifacts rendered with gitSync.enabled=false and other gitSync values set"

# 2. Enabling git-sync must not change any connector resource.
for tpl in deployment secret pvc serviceaccount; do
  before=$(helm template t "$chart" "${common[@]}" --show-only "templates/$tpl.yaml")
  after=$(helm template t "$chart" "${enabled[@]}" --show-only "templates/$tpl.yaml")
  [ "$before" = "$after" ] || fail "enabling git-sync changed the connector's $tpl.yaml"
done

# 3. A release whose stored values predate git-sync (Helm 3 `--reuse-values` replaces the
#    chart's defaults with the old release's values, so .Values.gitSync is absent). The old
#    values.yaml is this one without the gitSync block, which is the last block of the file.
old="$tmp/old-chart"
cp -R "$chart" "$old"
awk '/^# Optional git-sync add-on/ { exit } { print }' "$chart/values.yaml" >"$old/values.yaml"
if grep -q 'gitSync' "$old/values.yaml"; then fail "could not build the pre-git-sync values.yaml"; fi
# 3a. A plain connector upgrade still renders, identically.
[ "$(render_in "$old" "${common[@]}")" = "$(render "${common[@]}")" ] ||
  fail "connector render differs when the stored values have no gitSync"
refute "$(render_in "$old" "${common[@]}")" -i 'gitsync' "git-sync rendered from values without gitSync"
# 3b. A partial stored gitSync map (only the keys the user set) gets the defaults, and the
#     result is identical to rendering with the chart's own values.yaml.
for variant in "" "egress" "mounts"; do
  extra=()
  case $variant in
    egress) extra=(--set 'gitSync.networkPolicy.egress.cidrs={203.0.113.0/24}') ;;
    mounts) extra=(--set gitSync.caBundle.secretName=ca --set gitSync.providerConfig.secretName=prov
      --set gitSync.config.registrationTokenSecret.name=tok) ;;
  esac
  a=$(render_in "$old" "${enabled[@]}" "${extra[@]+"${extra[@]}"}")
  b=$(render "${enabled[@]}" "${extra[@]+"${extra[@]}"}")
  [ "$a" = "$b" ] || fail "git-sync defaults in the templates differ from values.yaml (variant '$variant')"
  [ -n "$(doc Deployment "$dep_name" <<<"$a")" ] || fail "no git-sync Deployment rendered from partial values"
done
# Booleans that default to true must still be switchable off from a partial map.
out=$(render_in "$old" "${enabled[@]}" --set gitSync.networkPolicy.enabled=false --set gitSync.serviceAccount.create=false)
[ -z "$(doc NetworkPolicy "$dep_name" <<<"$out")" ] || fail "networkPolicy.enabled=false ignored with partial values"
[ -z "$(doc ServiceAccount "$dep_name" <<<"$out")" ] || fail "serviceAccount.create=false ignored with partial values"
# 3c. A stored gitSync map that only says enabled=false (or a disabled add-on) renders nothing.
refute "$(render_in "$old" "${common[@]}" --set gitSync.enabled=false)" -i 'gitsync' "git-sync rendered from a disabled partial map"

# 4. Minimal enabled render: the expected objects, each selected by kind and name.
out=$(render "${enabled[@]}")
dep=$(doc Deployment "$dep_name" <<<"$out")
[ -n "$dep" ] || fail "no git-sync Deployment named <fullname>-gitsync"
pvc=$(doc PersistentVolumeClaim "$dep_name-data" <<<"$out")
[ -n "$pvc" ] || fail "no git-sync PersistentVolumeClaim"
sa=$(doc ServiceAccount "$dep_name" <<<"$out")
[ -n "$sa" ] || fail "no git-sync ServiceAccount"
np=$(doc NetworkPolicy "$dep_name" <<<"$out")
[ -n "$np" ] || fail "no git-sync NetworkPolicy"
check "$dep" '^  replicas: 1$' "git-sync replicas is not 1"
check "$dep" '^    type: Recreate$' "git-sync strategy is not Recreate"
check "$dep" "^          image: \"example/gitsync:$appver\"\$" "image tag does not default to Chart.appVersion"
check "$dep" '^      serviceAccountName: t-mkonnect-gitsync$' "git-sync Deployment does not use its dedicated ServiceAccount"
check "$dep" "^            claimName: $dep_name-data\$" "git-sync does not use its own PVC"
refute "$dep" 'claimName: t-mkonnect-data' "git-sync mounts the connector's PVC"
probe=$(section '          startupProbe:' <<<"$dep")
check "$probe" '^ *- /mkonnect-gitsync$' "startup probe does not run /mkonnect-gitsync"
check "$probe" '^ *- --health$' "startup probe does not pass --health"
check "$pvc" '^    - ReadWriteOnce$' "git-sync PVC access mode is not ReadWriteOnce"
refute "$pvc" 'ReadWriteMany|ReadOnlyMany|ReadWriteOncePod' "git-sync PVC has an access mode other than ReadWriteOnce"
check "$pvc" '^      storage: 20Gi$' "git-sync PVC size does not default to 20Gi"
check "$(env_var GATEWAY_URL <<<"$dep")" 'value: "wss://example.invalid/ws"' "gateway URL does not default to config.gatewayUrl"
check "$(env_var CONNECTOR_ID <<<"$dep")" "value: \"$gitsync_id\"" "git-sync connector ID not rendered"
refute "$dep" "$connector_id" "git-sync Deployment carries the connector's ID"
check "$dep" 'cpu: 250m' "default CPU request not rendered"
check "$dep" 'memory: 512Mi' "default memory request not rendered"
check "$dep" 'memory: 2Gi' "default memory limit not rendered"

# 5. Required values fail with a clear message.
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
expect_failure "must differ from config.connectorId" \
  "${enabled[@]}" --set config.connectorId=abcdef00-0000-0000-0000-000000000000 \
  --set gitSync.config.connectorId=ABCDEF00-0000-0000-0000-000000000000

# 6. replicaHost accepts what the connector accepts (host, host:port, IPv4, [IPv6], optional
#    port) and rejects a scheme, a path, userinfo and other shapes.
for good in replica.example.invalid replica.example.invalid:8443 replica Replica.Example.invalid \
  replica.example.invalid. under_score.example 192.0.2.10 192.0.2.10:8443 '[2001:db8::1]' '[2001:db8::1]:8443' \
  replica.example.invalid:65535; do
  render "${enabled[@]}" --set "gitSync.config.replicaHost=$good" >/dev/null 2>&1 ||
    fail "valid replicaHost '$good' was rejected"
done
for bad in https://replica.example.invalid replica.example.invalid/path replica.example.invalid/ \
  user@replica.example.invalid 'replica example' replica.example.invalid: replica.example.invalid:70000 \
  replica.example.invalid:0 replica.example.invalid:80:80 :443 2001:db8::1 '[2001:db8::1' '[host]:443' \
  192.0.2 256.1.1.1 0x7f000001 2130706433 a..b 'replica.example.invalid?x' 'replica.example.invalid#x' \
  'replica%2eexample' user@replica.example.invalid replica.example.invalid/; do
  expect_failure "gitSync.config.replicaHost" "${enabled[@]}" --set "gitSync.config.replicaHost=$bad"
done
dep=$(deployment "${enabled[@]}" --set gitSync.config.replicaHost=replica.example.invalid:8443)
check "$(env_var GIT_SYNC_REPLICA_HOST <<<"$dep")" 'value: "replica.example.invalid:8443"' "replicaHost with a port not rendered verbatim"

# 7. Image reference: a digest wins over the tag; a tag overrides the appVersion default.
digest=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
dep=$(deployment "${enabled[@]}" --set gitSync.image.tag=20260101.0 --set "gitSync.image.digest=$digest")
check "$dep" "^          image: \"example/gitsync@$digest\"\$" "digest does not win over tag"
refute "$dep" '20260101.0' "tag leaked into a digest-pinned image reference"
dep=$(deployment "${enabled[@]}" --set gitSync.image.tag=20260101.0)
check "$dep" '^          image: "example/gitsync:20260101.0"$' "explicit image tag not used"
expect_failure "gitSync.image.digest must look like" "${enabled[@]}" --set gitSync.image.digest=latest
# Numeric-looking values must not be turned into floats (2.0261022e+07) on the way through.
dep=$(deployment "${enabled[@]}" --set gitSync.image.tag=20261022 --set gitSync.tmp.sizeLimit=21474836480)
check "$dep" '^          image: "example/gitsync:20261022"$' "a numeric image tag was reformatted"
check "$(volume tmp <<<"$dep")" 'sizeLimit: "21474836480"' "a numeric tmp.sizeLimit was reformatted"

# 8. Security context, at the pod level and the container level separately.
dep=$(deployment "${enabled[@]}")
check "$dep" '^      automountServiceAccountToken: false$' "pod token automount not disabled"
check "$dep" '^      enableServiceLinks: false$' "enableServiceLinks is not false"
pod=$(pod_security <<<"$dep")
for want in 'runAsNonRoot: true' 'runAsUser: 65532' 'runAsGroup: 65532' 'fsGroup: 65532' \
  'fsGroupChangePolicy: OnRootMismatch'; do
  check "$pod" "^        $want\$" "pod securityContext lacks: $want"
done
check "$(section '        seccompProfile:' <<<"$pod")" '^          type: RuntimeDefault$' "pod securityContext lacks the RuntimeDefault seccomp profile"
ctr=$(container_security <<<"$dep")
for want in 'runAsNonRoot: true' 'allowPrivilegeEscalation: false' 'readOnlyRootFilesystem: true'; do
  check "$ctr" "^ *$want\$" "container securityContext lacks: $want"
done
check "$(section '            seccompProfile:' <<<"$ctr")" '^              type: RuntimeDefault$' "container securityContext lacks the RuntimeDefault seccomp profile"
check "$(section '            capabilities:' <<<"$ctr")" '^ *- ALL$' "capabilities.drop is not [ALL]"
drops=$(section '            capabilities:' <<<"$ctr" | grep -c '^ *- ')
[ "$drops" = 1 ] || fail "capabilities.drop has more than ALL"
refute "$dep" 'privileged: true' "privileged container"
check "$sa" '^automountServiceAccountToken: false$' "ServiceAccount token is automounted"

# 9. Disk-backed /tmp with a size limit, TMPDIR and GIT_SYNC_TMP_DIR pointing at it.
check "$(mount tmp <<<"$dep")" 'mountPath: /tmp$' "/tmp not mounted from the tmp volume"
check "$(env_var TMPDIR <<<"$dep")" 'value: "/tmp"' "TMPDIR is not /tmp"
check "$(env_var GIT_SYNC_TMP_DIR <<<"$dep")" 'value: "/tmp"' "GIT_SYNC_TMP_DIR is not /tmp"
tmpvol=$(volume tmp <<<"$dep")
check "$tmpvol" 'emptyDir:' "/tmp is not an emptyDir"
check "$tmpvol" 'sizeLimit: "5Gi"' "/tmp sizeLimit does not default to 5Gi"
refute "$dep" 'medium' "an emptyDir is memory-backed"
dep2=$(deployment "${enabled[@]}" --set gitSync.tmp.sizeLimit=40Gi)
check "$(volume tmp <<<"$dep2")" 'sizeLimit: "40Gi"' "tmp.sizeLimit override not rendered"

# 10. Explicit paths: key, mirrors, and the default provider config on the data volume.
check "$(env_var KEY_FILE <<<"$dep")" 'value: "/data/gitsync-key"' "KEY_FILE not /data/gitsync-key"
check "$(env_var GIT_SYNC_MIRRORS_DIR <<<"$dep")" 'value: "/data/mirrors"' "mirrors dir is not /data/mirrors"
check "$(env_var GIT_SYNC_CONFIG_FILE <<<"$dep")" 'value: "/data/git-sync.json"' "GIT_SYNC_CONFIG_FILE does not default to /data/git-sync.json"
check "$(mount data <<<"$dep")" 'mountPath: /data$' "data volume not mounted at /data"
refute "$dep" 'name: provider-config' "provider-config mount rendered without a Secret"
for key in /etc/key /data /data/git-sync.json /data/mirrors /data/mirrors/key relative; do
  expect_failure "gitSync.config.keyFile" "${enabled[@]}" --set "gitSync.config.keyFile=$key"
done
dep2=$(deployment "${enabled[@]}" --set gitSync.config.keyFile=/data/other-key)
check "$(env_var KEY_FILE <<<"$dep2")" 'value: "/data/other-key"' "keyFile override not rendered"

# 11. Provider config Secret: mounted read-only, 0440, outside /data; mirrors stay on /data.
dep=$(deployment "${enabled[@]}" --set gitSync.providerConfig.secretName=my-providers)
check "$(env_var GIT_SYNC_CONFIG_FILE <<<"$dep")" 'value: "/etc/mkonnect-gitsync-providers/git-sync.json"' "GIT_SYNC_CONFIG_FILE does not point at the mounted Secret"
check "$(env_var GIT_SYNC_MIRRORS_DIR <<<"$dep")" 'value: "/data/mirrors"' "mirrors dir moved when a provider Secret is used"
vol=$(volume provider-config <<<"$dep")
check "$vol" 'secretName: "my-providers"' "provider-config Secret not mounted"
check "$vol" 'defaultMode: 0440' "provider-config Secret defaultMode is not 0440"
mnt=$(mount provider-config <<<"$dep")
check "$mnt" 'mountPath: /etc/mkonnect-gitsync-providers$' "provider-config mountPath wrong"
check "$mnt" 'readOnly: true' "provider-config mount is not read-only"

# 12. Registration token: a read-only mounted file, never an environment variable.
dep=$(deployment "${enabled[@]}")
refute "$dep" 'name: REGISTRATION_TOKEN$' "REGISTRATION_TOKEN env var rendered"
refute "$dep" 'REGISTRATION_TOKEN_FILE' "token file env rendered with no token source"
refute "$(render "${enabled[@]}")" 'gitsync-registration' "token Secret rendered with no token supplied"
dep=$(deployment "${enabled[@]}" --set gitSync.config.registrationToken=s3cret-value)
refute "$dep" 'name: REGISTRATION_TOKEN$' "REGISTRATION_TOKEN env var rendered"
refute "$dep" 's3cret-value' "token value leaked into the Deployment"
check "$(env_var REGISTRATION_TOKEN_FILE <<<"$dep")" 'value: "/etc/mkonnect-gitsync/registration-token/registration-token"' "token file env missing or wrong"
vol=$(volume registration-token <<<"$dep")
check "$vol" 'secretName: "t-mkonnect-gitsync-registration"' "chart-managed token Secret not mounted"
check "$vol" 'defaultMode: 0440' "token Secret defaultMode is not 0440"
mnt=$(mount registration-token <<<"$dep")
check "$mnt" 'mountPath: /etc/mkonnect-gitsync/registration-token$' "token mountPath wrong"
check "$mnt" 'readOnly: true' "token mount is not read-only"
sec=$(render "${enabled[@]}" --set gitSync.config.registrationToken=s3cret-value | doc Secret "$dep_name-registration")
check "$sec" "registration-token: \"$(printf %s s3cret-value | base64)\"" "chart-managed token Secret content wrong"
# An existing Secret wins over a value and no chart-managed Secret is created.
out=$(render "${enabled[@]}" --set gitSync.config.registrationToken=s3cret-value \
  --set gitSync.config.registrationTokenSecret.name=my-token --set gitSync.config.registrationTokenSecret.key=tok)
dep=$(doc Deployment "$dep_name" <<<"$out")
vol=$(volume registration-token <<<"$dep")
check "$vol" 'secretName: "my-token"' "referenced token Secret not mounted"
check "$vol" 'defaultMode: 0440' "referenced token Secret defaultMode is not 0440"
check "$(env_var REGISTRATION_TOKEN_FILE <<<"$dep")" 'value: "/etc/mkonnect-gitsync/registration-token/tok"' "token file path ignores registrationTokenSecret.key"
refute "$out" 't-mkonnect-gitsync-registration' "chart-managed token Secret created alongside a referenced one"

# 13. NetworkPolicy: ingress denied for the git-sync pods; egress untouched by default.
selector=$(section '  podSelector:' <<<"$np")
check "$selector" '^      app.kubernetes.io/name: mkonnect-gitsync$' "NetworkPolicy podSelector does not select the git-sync pods by name"
check "$selector" '^      app.kubernetes.io/instance: t$' "NetworkPolicy podSelector does not select the release's pods"
refute "$selector" '\{\}' "NetworkPolicy podSelector is empty (selects every pod)"
policy_types=$(section '  policyTypes:' <<<"$np")
check "$policy_types" '^    - Ingress$' "NetworkPolicy lacks the Ingress policyType"
refute "$policy_types" 'Egress' "egress restricted by default"
refute "$np" '^  (ingress|egress):' "NetworkPolicy has ingress/egress rules by default"
out=$(render "${enabled[@]}" --set gitSync.networkPolicy.enabled=false)
[ -z "$(doc NetworkPolicy "$dep_name" <<<"$out")" ] || fail "NetworkPolicy rendered while disabled"
# The git-sync and connector pods must not select one another.
selector_dep=$(section '    matchLabels:' <<<"$(section '  selector:' <<<"$(deployment "${enabled[@]}")")")
check "$selector_dep" 'app.kubernetes.io/name: mkonnect-gitsync$' "git-sync Deployment selector wrong"
refute "$selector_dep" 'app.kubernetes.io/name: mkonnect$' "git-sync pods carry the connector's selector label"

# 14. Optional egress allow-list: CIDRs on ports, plus DNS.
np=$(render "${enabled[@]}" --set 'gitSync.networkPolicy.egress.cidrs={203.0.113.0/24,198.51.100.7/32}' | doc NetworkPolicy "$dep_name")
check "$(section '  policyTypes:' <<<"$np")" '^    - Egress$' "Egress policyType missing when cidrs set"
check "$np" 'cidr: "203.0.113.0/24"' "first egress CIDR missing"
check "$np" 'cidr: "198.51.100.7/32"' "second egress CIDR missing"
check "$np" 'protocol: TCP' "egress TCP rule missing"
check "$np" 'port: 443$' "egress does not default to port 443"
check "$np" 'port: 53$' "DNS egress missing"
check "$np" 'protocol: UDP' "DNS over UDP missing"
check "$np" 'kubernetes.io/metadata.name: "kube-system"' "DNS namespace selector missing"
np=$(render "${enabled[@]}" --set 'gitSync.networkPolicy.egress.cidrs={203.0.113.0/24}' \
  --set 'gitSync.networkPolicy.egress.ports={443,8443}' | doc NetworkPolicy "$dep_name")
check "$np" 'port: 8443$' "custom egress port missing"

# 15. No Service and no container port: git-sync accepts no inbound connections.
out=$(render "${enabled[@]}" --set 'gitSync.networkPolicy.egress.cidrs={203.0.113.0/24}')
refute "$out" '^kind: Service$' "a Service is rendered"
refute "$(deployment "${enabled[@]}")" 'containerPort|hostPort|hostNetwork|hostPID|hostIPC' "git-sync Deployment exposes a port or shares a host namespace"

# 16. Custom CA bundle for git: GIT_SSL_CAINFO, 0444 read-only mount, no overlap with other mounts.
dep=$(deployment "${enabled[@]}")
refute "$dep" 'GIT_SSL_CAINFO' "GIT_SSL_CAINFO set without caBundle"
dep=$(deployment "${enabled[@]}" --set gitSync.caBundle.secretName=my-ca)
check "$(env_var GIT_SSL_CAINFO <<<"$dep")" 'value: "/etc/mkonnect-gitsync-ca/ca-bundle.pem"' "GIT_SSL_CAINFO value wrong"
vol=$(volume ca-bundle <<<"$dep")
check "$vol" 'secretName: "my-ca"' "CA Secret not mounted"
check "$vol" 'defaultMode: 0444' "CA Secret defaultMode is not 0444"
mnt=$(mount ca-bundle <<<"$dep")
check "$mnt" 'readOnly: true' "CA mount is not read-only"
for bad in /data /data/ca /tmp /tmp/ca / /etc /etc/ssl /etc/ssl/certs /etc/mkonnect-gitsync /etc/mkonnect-gitsync/registration-token \
  /etc/mkonnect-gitsync/registration-token/ca /etc/mkonnect-gitsync-providers /etc/mkonnect-gitsync-providers/ca \
  /mkonnect-gitsync /bin /sbin /usr /usr/local/bin /lib /lib64 /proc /dev /sys relative/path; do
  expect_failure "gitSync.caBundle.mountPath" "${enabled[@]}" \
    --set gitSync.caBundle.secretName=my-ca --set "gitSync.caBundle.mountPath=$bad"
done
# A sibling that merely shares a name prefix with another mount is fine.
for ok in /etc/mkonnect-gitsync-ca2 /opt/certs /etc/mkonnect-gitsync-providers2; do
  deployment "${enabled[@]}" --set gitSync.caBundle.secretName=my-ca \
    --set "gitSync.caBundle.mountPath=$ok" >/dev/null 2>&1 || fail "non-overlapping CA mountPath $ok rejected"
done

# 17. With every optional mount enabled, no two container mount paths overlap, and the
#     connector's own CA and credentials mounts coexist.
out=$(render "${enabled[@]}" --set gitSync.caBundle.secretName=my-ca \
  --set gitSync.providerConfig.secretName=my-providers \
  --set gitSync.config.registrationTokenSecret.name=my-token \
  --set caBundle.secretName=conn-ca --set credentials.secretName=conn-creds)
check "$(doc Deployment t-mkonnect <<<"$out")" 'name: CA_BUNDLE' "connector CA_BUNDLE lost when git-sync is enabled"
dep=$(doc Deployment "$dep_name" <<<"$out")
mounts=()
while IFS= read -r line; do
  mounts+=("$(sed -e 's/^ *mountPath: //' -e 's/"//g' <<<"$line")")
done < <(section '          volumeMounts:' <<<"$dep" | grep 'mountPath:')
[ "${#mounts[@]}" -ge 5 ] || fail "expected at least 5 git-sync mounts, found ${#mounts[@]}"
for ((i = 0; i < ${#mounts[@]}; i++)); do
  for ((j = i + 1; j < ${#mounts[@]}; j++)); do
    if overlaps "${mounts[$i]}" "${mounts[$j]}"; then
      fail "git-sync mount paths overlap: ${mounts[$i]} and ${mounts[$j]}"
    fi
  done
done

# 18. Proxy settings are exported only when set.
dep=$(deployment "${enabled[@]}")
refute "$dep" 'PROXY' "proxy variables rendered by default"
dep=$(deployment "${enabled[@]}" --set gitSync.proxy.https=http://proxy.example.invalid:3128 \
  --set gitSync.proxy.http=http://proxy.example.invalid:3128 --set gitSync.proxy.noProxy=.cluster.local)
check "$(env_var HTTPS_PROXY <<<"$dep")" 'value: "http://proxy.example.invalid:3128"' "HTTPS_PROXY not rendered"
check "$(env_var HTTP_PROXY <<<"$dep")" 'value: "http://proxy.example.invalid:3128"' "HTTP_PROXY not rendered"
check "$(env_var NO_PROXY <<<"$dep")" 'value: ".cluster.local"' "NO_PROXY not rendered"

# 19. Naming overrides and the dedicated ServiceAccount.
out=$(helm template rel "$chart" "${enabled[@]}" --set fullnameOverride=custom)
[ -n "$(doc Deployment custom-gitsync <<<"$out")" ] || fail "fullnameOverride not honoured"
out=$(render "${enabled[@]}" --set gitSync.serviceAccount.create=false)
[ -z "$(doc ServiceAccount "$dep_name" <<<"$out")" ] || fail "git-sync ServiceAccount rendered with create=false"
check "$(doc Deployment "$dep_name" <<<"$out")" '^      serviceAccountName: default$' "serviceAccount.create=false did not fall back to default"

# 20. NOTES mention git-sync only when enabled. NOTES.txt is rendered here as an ordinary
#     template in a scratch copy of the chart, so no release (and no cluster) is needed.
notes() {
  local d="$tmp/notes-chart"
  rm -rf "$d"
  cp -R "$chart" "$d"
  {
    printf '{{- define "t.notes" -}}\n'
    cat "$chart/templates/NOTES.txt"
    printf '\n{{- end -}}\n'
  } >"$d/templates/_notes.tpl"
  printf 'notes: |\n{{ include "t.notes" . | indent 2 }}\n' >"$d/templates/notes.yaml"
  helm template t "$d" -n ns1 "$@" --show-only templates/notes.yaml
}
text=$(notes "${enabled[@]}")
check "$text" 'git-sync is enabled as deployment t-mkonnect-gitsync' "NOTES lack the git-sync section when enabled"
check "$text" 'kubectl -n ns1 exec -i deploy/t-mkonnect-gitsync' "NOTES kubectl commands lack the namespace"
check "$text" 'connector config set' "NOTES do not show how to configure a provider"
text=$(notes "${enabled[@]}" --set gitSync.providerConfig.secretName=my-providers)
check "$text" 'mounted read-only from the Secret my-providers' "NOTES do not mention the provider config Secret"
refute "$(notes "${common[@]}")" -i 'git-sync' "NOTES mention git-sync while disabled"

printf 'chart git-sync tests passed\n'
