{{/*
Expand the name of the chart.
*/}}
{{- define "mkonnect.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "mkonnect.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "mkonnect.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "mkonnect.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "mkonnect.selectorLabels" -}}
app.kubernetes.io/name: {{ include "mkonnect.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Service account name
*/}}
{{- define "mkonnect.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "mkonnect.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
git-sync add-on. Its pods carry their own app.kubernetes.io/name so the connector
Deployment's selector never matches them (and the reverse).
*/}}
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
{{- if .Values.gitSync.serviceAccount.create }}
{{- default (include "mkonnect.gitsync.fullname" .) .Values.gitSync.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.gitSync.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Container image reference. A digest, when set, wins over the tag.
*/}}
{{- define "mkonnect.gitsync.image" -}}
{{- $repo := required "gitSync.image.repository is required when gitSync.enabled is true (e.g. public.ecr.aws/<alias>/memento-gitsync)" .Values.gitSync.image.repository -}}
{{- if .Values.gitSync.image.digest -}}
{{- if not (regexMatch "^sha256:[a-f0-9]{64}$" .Values.gitSync.image.digest) -}}
{{- fail "gitSync.image.digest must look like sha256:<64 lowercase hex characters>" -}}
{{- end -}}
{{- printf "%s@%s" $repo .Values.gitSync.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repo (.Values.gitSync.image.tag | default .Chart.AppVersion) -}}
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
Fail the render, with a clear message, for any required value that is missing or malformed.
*/}}
{{- define "mkonnect.gitsync.validate" -}}
{{- $gs := .Values.gitSync -}}
{{- $connectorId := required "gitSync.config.connectorId is required when gitSync.enabled is true: use the connector ID issued for the git-sync connector" $gs.config.connectorId -}}
{{- if eq $connectorId .Values.config.connectorId -}}
{{- fail "gitSync.config.connectorId must differ from config.connectorId: the git-sync connector needs its own connector ID" -}}
{{- end -}}
{{- $_ := required "gitSync.config.gatewayUrl (or config.gatewayUrl) is required when gitSync.enabled is true" (default .Values.config.gatewayUrl $gs.config.gatewayUrl) -}}
{{- $host := required "gitSync.config.replicaHost is required when gitSync.enabled is true: use the replica host shown in the portal install page" $gs.config.replicaHost -}}
{{- if not (regexMatch "^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*(:[0-9]{1,5})?$" $host) -}}
{{- fail (printf "gitSync.config.replicaHost must be a host name or host:port with no scheme and no path (got %q)" $host) -}}
{{- end -}}
{{- if contains ":" $host -}}
{{- $port := int (last (splitList ":" $host)) -}}
{{- if or (lt $port 1) (gt $port 65535) -}}
{{- fail (printf "gitSync.config.replicaHost has an invalid port (got %q)" $host) -}}
{{- end -}}
{{- end -}}
{{- $_ := include "mkonnect.gitsync.image" . -}}
{{- if not (hasPrefix "/data/" (clean $gs.config.keyFile)) -}}
{{- fail "gitSync.config.keyFile must be a path under /data, the only writable persistent location" -}}
{{- end -}}
{{- if $gs.providerConfig.secretName -}}
{{- $_ := required "gitSync.providerConfig.key is required when gitSync.providerConfig.secretName is set" $gs.providerConfig.key -}}
{{- end -}}
{{- if $gs.config.registrationTokenSecret.name -}}
{{- $_ := required "gitSync.config.registrationTokenSecret.key is required when gitSync.config.registrationTokenSecret.name is set" $gs.config.registrationTokenSecret.key -}}
{{- end -}}
{{- if $gs.caBundle.secretName -}}
{{- $ca := required "gitSync.caBundle.mountPath is required when gitSync.caBundle.secretName is set" $gs.caBundle.mountPath -}}
{{- $_ := required "gitSync.caBundle.key is required when gitSync.caBundle.secretName is set" $gs.caBundle.key -}}
{{- if not (hasPrefix "/" $ca) -}}
{{- fail "gitSync.caBundle.mountPath must be an absolute path" -}}
{{- end -}}
{{- range $other := list "/data" "/tmp" "/etc/mkonnect-gitsync/registration-token" "/etc/mkonnect-gitsync-providers" -}}
{{- if include "mkonnect.gitsync.pathsOverlap" (list $ca $other) -}}
{{- fail (printf "gitSync.caBundle.mountPath must not overlap the git-sync data, tmp, registration-token or provider-config mounts (%s overlaps %s)" $ca $other) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}
