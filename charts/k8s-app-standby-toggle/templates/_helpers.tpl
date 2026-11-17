{{- define "standby.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "standby.fullname" -}}
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

{{- define "standby.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "standby.labels" -}}
helm.sh/chart: {{ include "standby.chart" . }}
{{ include "standby.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "standby.selectorLabels" -}}
app.kubernetes.io/name: {{ include "standby.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "standby.serviceAccountName" -}}
{{- default (include "standby.fullname" .) .Values.serviceAccount.name }}
{{- end }}

{{/* The runtime state ConfigMap; created by the service, not the chart. */}}
{{- define "standby.stateConfigMap" -}}
{{- printf "%s-state" (include "standby.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "standby.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) }}
{{- end }}
{{- end }}

{{- define "standby.redirectURL" -}}
{{- if .Values.oidc.redirectURL }}
{{- .Values.oidc.redirectURL }}
{{- else if and .Values.httpRoute.enabled .Values.httpRoute.hostnames }}
{{- printf "https://%s/auth/callback" (first .Values.httpRoute.hostnames) }}
{{- end }}
{{- end }}

{{- define "standby.validate" -}}
{{- if not .Values.groups }}
{{- fail "groups: configure at least one group" }}
{{- end }}
{{- $owner := dict }}
{{- range $group := keys .Values.groups | sortAlpha }}
{{- range (get $.Values.groups $group).apps }}
{{- if hasKey $owner . }}
{{- fail (printf "groups: Application %q is listed in both group %q and group %q" . (get $owner .) $group) }}
{{- end }}
{{- $_ := set $owner . $group }}
{{- end }}
{{- end }}
{{- if not .Values.oidc.issuerURL }}
{{- fail "oidc.issuerURL is required" }}
{{- end }}
{{- if not (include "standby.redirectURL" .) }}
{{- fail "oidc.redirectURL is required (or enable httpRoute with a hostname)" }}
{{- end }}
{{- if and (not .Values.oidc.allowedGroups) (not .Values.oidc.allowAnyUser) }}
{{- fail "oidc.allowedGroups is empty: list the groups that may use the service, or set oidc.allowAnyUser" }}
{{- end }}
{{- if and .Values.oidc.ca.configMap .Values.oidc.ca.secret }}
{{- fail "oidc.ca: set configMap or secret, not both" }}
{{- end }}
{{- if and .Values.httpRoute.enabled (not .Values.httpRoute.parentRefs) }}
{{- fail "httpRoute.parentRefs is required when httpRoute.enabled" }}
{{- end }}
{{- end }}
