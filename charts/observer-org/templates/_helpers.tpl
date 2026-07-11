{{/* Expand the name of the chart. */}}
{{- define "observer-org.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Fully qualified app name. */}}
{{- define "observer-org.fullname" -}}
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

{{- define "observer-org.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "observer-org.labels" -}}
helm.sh/chart: {{ include "observer-org.chart" . }}
{{ include "observer-org.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "observer-org.selectorLabels" -}}
app.kubernetes.io/name: {{ include "observer-org.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "observer-org.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "observer-org.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* The image reference, defaulting the tag to the chart appVersion. */}}
{{- define "observer-org.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end }}

{{/* The PVC name (existing claim or chart-managed). */}}
{{- define "observer-org.pvcName" -}}
{{- if .Values.persistence.existingClaim }}
{{- .Values.persistence.existingClaim }}
{{- else }}
{{- printf "%s-data" (include "observer-org.fullname" .) }}
{{- end }}
{{- end }}
