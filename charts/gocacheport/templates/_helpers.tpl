{{- define "gocacheport.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "gocacheport.fullname" -}}
{{- default (printf "%s-%s" .Release.Name (include "gocacheport.name" .)) .Values.fullnameOverride | trunc 55 | trimSuffix "-" -}}
{{- end -}}

{{- define "gocacheport.selectorLabels" -}}
app.kubernetes.io/name: {{ include "gocacheport.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "gocacheport.labels" -}}
{{ include "gocacheport.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end -}}
