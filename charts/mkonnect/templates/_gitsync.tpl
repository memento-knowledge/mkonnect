{{/*
git-sync add-on helpers.

Its pods carry their own app.kubernetes.io/name so the connector Deployment's selector never
matches them (and the reverse).

Every git-sync template reads its settings through "mkonnect.gitsync.values", never through
.Values.gitSync directly. Helm 3's `helm upgrade --reuse-values` replaces the new chart's defaults
with the previous release's values, so for a release created before git-sync existed
.Values.gitSync is absent (and a partial gitSync map may lack keys). The helper therefore treats a
missing map as disabled and fills any missing key from the defaults below. Those defaults mirror
the gitSync block in values.yaml and are kept in step with it by scripts/chart-gitsync_test.sh.
*/}}

{{/* Built-in defaults: must equal the gitSync block in values.yaml (except `enabled`). */}}
{{- define "mkonnect.gitsync.defaults" -}}
image:
  repository: ""
  pullPolicy: IfNotPresent
  tag: ""
  digest: ""
config:
  gatewayUrl: ""
  connectorId: ""
  protocolVersion: ""
  replicaHost: ""
  keyFile: /data/gitsync-key
  registrationTokenSecret:
    name: ""
    key: registration-token
  registrationToken: ""
providerConfig:
  secretName: ""
  key: git-sync.json
caBundle:
  secretName: ""
  key: ca-bundle.pem
  mountPath: /etc/mkonnect-gitsync-ca
persistence:
  size: 20Gi
  storageClass: ""
tmp:
  sizeLimit: 5Gi
resources:
  requests:
    cpu: 250m
    memory: 512Mi
  limits:
    cpu: "1"
    memory: 2Gi
proxy:
  https: ""
  http: ""
  noProxy: ""
serviceAccount:
  create: true
  name: ""
networkPolicy:
  enabled: true
  egress:
    cidrs: []
    ports:
      - 443
    dnsNamespace: kube-system
{{- end }}

{{/* "true" when git-sync is enabled, empty otherwise. Safe when .Values.gitSync is absent. */}}
{{- define "mkonnect.gitsync.enabled" -}}
{{- if dig "enabled" false (.Values.gitSync | default dict) -}}true{{- end -}}
{{- end }}

{{/*
The effective git-sync settings as YAML: the user's values over the defaults. Use as
`include "mkonnect.gitsync.values" . | fromYaml`.
*/}}
{{- define "mkonnect.gitsync.values" -}}
{{- $user := deepCopy (.Values.gitSync | default dict) -}}
{{- /* merge treats false as unset, so take the booleans from the user's input directly. */ -}}
{{- $enabled := dig "enabled" false $user -}}
{{- $npEnabled := dig "networkPolicy" "enabled" true $user -}}
{{- $saCreate := dig "serviceAccount" "create" true $user -}}
{{- $merged := merge $user (fromYaml (include "mkonnect.gitsync.defaults" .)) -}}
{{- $_ := set $merged "enabled" $enabled -}}
{{- $_ = set (get $merged "networkPolicy") "enabled" $npEnabled -}}
{{- $_ = set (get $merged "serviceAccount") "create" $saCreate -}}
{{- /*
  The YAML round trip in the callers turns numbers into floats, which print like 2.0261022e+07.
  Make every scalar that is printed verbatim a string first, so a numeric-looking value (a tag
  or a size in bytes given with --set) survives.
*/ -}}
{{- range $p := list (list "image" "repository") (list "image" "pullPolicy") (list "image" "tag") (list "image" "digest") (list "config" "gatewayUrl") (list "config" "connectorId") (list "config" "protocolVersion") (list "config" "replicaHost") (list "config" "keyFile") (list "config" "registrationToken") (list "providerConfig" "secretName") (list "providerConfig" "key") (list "caBundle" "secretName") (list "caBundle" "key") (list "caBundle" "mountPath") (list "persistence" "size") (list "persistence" "storageClass") (list "tmp" "sizeLimit") (list "serviceAccount" "name") (list "proxy" "https") (list "proxy" "http") (list "proxy" "noProxy") -}}
{{- $section := get $merged (first $p) -}}
{{- $_ = set $section (last $p) (toString (get $section (last $p))) -}}
{{- end -}}
{{- $tokenSecret := get (get $merged "config") "registrationTokenSecret" -}}
{{- $_ = set $tokenSecret "name" (toString (get $tokenSecret "name")) -}}
{{- $_ = set $tokenSecret "key" (toString (get $tokenSecret "key")) -}}
{{- toYaml $merged -}}
{{- end }}

{{- define "mkonnect.gitsync.fullname" -}}
{{- printf "%s-gitsync" (include "mkonnect.fullname" . | trunc 52 | trimSuffix "-") }}
{{- end }}

{{- define "mkonnect.gitsync.selectorLabels" -}}
app.kubernetes.io/name: {{ printf "%s-gitsync" (include "mkonnect.name" . | trunc 52 | trimSuffix "-") }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "mkonnect.gitsync.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "mkonnect.gitsync.selectorLabels" . }}
app.kubernetes.io/component: gitsync
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "mkonnect.gitsync.serviceAccountName" -}}
{{- $gs := include "mkonnect.gitsync.values" . | fromYaml -}}
{{- if $gs.serviceAccount.create }}
{{- default (include "mkonnect.gitsync.fullname" .) $gs.serviceAccount.name }}
{{- else }}
{{- default "default" $gs.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Container image reference. A digest, when set, wins over the tag.
*/}}
{{- define "mkonnect.gitsync.image" -}}
{{- $gs := include "mkonnect.gitsync.values" . | fromYaml -}}
{{- $repo := required "gitSync.image.repository is required when gitSync.enabled is true (e.g. public.ecr.aws/<alias>/memento-gitsync)" $gs.image.repository -}}
{{- if $gs.image.digest -}}
{{- if not (regexMatch "^sha256:[a-f0-9]{64}$" $gs.image.digest) -}}
{{- fail "gitSync.image.digest must look like sha256:<64 lowercase hex characters>" -}}
{{- end -}}
{{- printf "%s@%s" $repo $gs.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repo ($gs.image.tag | default .Chart.AppVersion) -}}
{{- end -}}
{{- end }}

{{/*
True (non-empty) when two absolute paths are equal or one is nested under the other.
Takes a list of two paths.
*/}}
{{- define "mkonnect.gitsync.pathsOverlap" -}}
{{- $a := index . 0 | clean -}}
{{- $b := index . 1 | clean -}}
{{- if or (eq $a $b) (hasPrefix (printf "%s/" (trimSuffix "/" $a)) $b) (hasPrefix (printf "%s/" (trimSuffix "/" $b)) $a) -}}true{{- end -}}
{{- end }}

{{/*
"true" when the argument is an acceptable replica host: a host name, IPv4 address or bracketed IPv6
address, optionally followed by :port (1-65535), with no scheme, path, userinfo or whitespace. This
follows the syntax the connector accepts for GIT_SYNC_REPLICA_HOST: host names are ASCII letters,
digits, hyphen and underscore in labels of at most 63 characters, with a numeric final label rejected
(it would be read as an address). The connector applies the authoritative check at startup.
*/}}
{{- define "mkonnect.gitsync.validReplicaHost" -}}
{{- $h := . | toString -}}
{{- $ok := false -}}
{{- $host := "" -}}
{{- $port := "" -}}
{{- if and (le (len $h) 262) (regexMatch "^[!-~]+$" $h) (not (contains "://" $h)) -}}
{{- if hasPrefix "[" $h -}}
{{- if regexMatch "^\\[[0-9A-Fa-f:.]*:[0-9A-Fa-f:.]*\\](:[0-9]+)?$" $h -}}
{{- $host = regexFind "^\\[[^\\]]*\\]" $h -}}
{{- $port = trimPrefix ":" (trimPrefix $host $h) -}}
{{- $ok = true -}}
{{- end -}}
{{- else if le (len (splitList ":" $h)) 2 -}}
{{- $host = first (splitList ":" $h) -}}
{{- $port = ternary (last (splitList ":" $h)) "" (eq (len (splitList ":" $h)) 2) -}}
{{- $name := trimSuffix "." $host -}}
{{- if regexMatch "^(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])(\\.(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])){3}$" $name -}}
{{- $ok = true -}}
{{- else if and (ne $name "") (regexMatch "^[A-Za-z0-9_-]{1,63}(\\.[A-Za-z0-9_-]{1,63})*$" $name) (not (regexMatch "^([0-9]+|0[xX][0-9a-fA-F]*)$" (last (splitList "." $name)))) -}}
{{- $ok = true -}}
{{- end -}}
{{- end -}}
{{- if and $ok (contains ":" (trimPrefix $host $h)) -}}
{{- if not (regexMatch "^[0-9]{1,5}$" $port) -}}
{{- $ok = false -}}
{{- else if or (lt (atoi $port) 1) (gt (atoi $port) 65535) -}}
{{- $ok = false -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if $ok -}}true{{- end -}}
{{- end }}

{{/*
Fail the render, with a clear message, for any required value that is missing or malformed.
*/}}
{{- define "mkonnect.gitsync.validate" -}}
{{- $gs := include "mkonnect.gitsync.values" . | fromYaml -}}
{{- $connectorId := required "gitSync.config.connectorId is required when gitSync.enabled is true: use the connector ID issued for the git-sync connector" $gs.config.connectorId | toString -}}
{{- if eq (lower $connectorId) (lower (toString .Values.config.connectorId)) -}}
{{- fail "gitSync.config.connectorId must differ from config.connectorId: the git-sync connector needs its own connector ID" -}}
{{- end -}}
{{- $_ := required "gitSync.config.gatewayUrl (or config.gatewayUrl) is required when gitSync.enabled is true" (default .Values.config.gatewayUrl $gs.config.gatewayUrl) -}}
{{- $host := required "gitSync.config.replicaHost is required when gitSync.enabled is true: use the replica host from your installation instructions" $gs.config.replicaHost | toString -}}
{{- if not (include "mkonnect.gitsync.validReplicaHost" $host) -}}
{{- fail (printf "gitSync.config.replicaHost must be a host name, IP address or [IPv6]:port, optionally with :port, and no scheme, path or userinfo (got %q)" $host) -}}
{{- end -}}
{{- $_ = include "mkonnect.gitsync.image" . -}}
{{- $key := clean $gs.config.keyFile -}}
{{- if not (hasPrefix "/data/" $key) -}}
{{- fail "gitSync.config.keyFile must be a path under /data, the only writable persistent location" -}}
{{- end -}}
{{- if or (eq $key "/data/git-sync.json") (eq $key "/data/mirrors") (hasPrefix "/data/mirrors/" $key) -}}
{{- fail "gitSync.config.keyFile must not be the provider config file (/data/git-sync.json) or inside the mirrors directory (/data/mirrors)" -}}
{{- end -}}
{{- if $gs.providerConfig.secretName -}}
{{- $_ = required "gitSync.providerConfig.key is required when gitSync.providerConfig.secretName is set" $gs.providerConfig.key -}}
{{- end -}}
{{- if $gs.config.registrationTokenSecret.name -}}
{{- $_ = required "gitSync.config.registrationTokenSecret.key is required when gitSync.config.registrationTokenSecret.name is set" $gs.config.registrationTokenSecret.key -}}
{{- end -}}
{{- if $gs.caBundle.secretName -}}
{{- $ca := required "gitSync.caBundle.mountPath is required when gitSync.caBundle.secretName is set" $gs.caBundle.mountPath -}}
{{- $_ = required "gitSync.caBundle.key is required when gitSync.caBundle.secretName is set" $gs.caBundle.key -}}
{{- if not (hasPrefix "/" $ca) -}}
{{- fail "gitSync.caBundle.mountPath must be an absolute path" -}}
{{- end -}}
{{- /* Never overlap another mount of the pod, and never shadow the binary or system directories. */ -}}
{{- range $other := list "/data" "/tmp" "/etc/mkonnect-gitsync/registration-token" "/etc/mkonnect-gitsync-providers" "/mkonnect-gitsync" "/bin" "/sbin" "/usr" "/lib" "/lib64" "/proc" "/dev" "/sys" -}}
{{- if include "mkonnect.gitsync.pathsOverlap" (list $ca $other) -}}
{{- fail (printf "gitSync.caBundle.mountPath must not overlap another git-sync mount, the git-sync binary, or a system directory (%s overlaps %s)" $ca $other) -}}
{{- end -}}
{{- end -}}
{{- range $other := list "/etc" "/etc/ssl" "/etc/ssl/certs" -}}
{{- if eq (clean $ca) $other -}}
{{- fail (printf "gitSync.caBundle.mountPath must not be %s: it would hide the system files there" $other) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}
