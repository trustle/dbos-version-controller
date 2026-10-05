{{- define "dvc.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "dvc.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else if contains (include "dvc.name" .) .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name (include "dvc.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "dvc.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "dvc.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- required "serviceAccount.name is required when serviceAccount.create is false" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "dvc.watchNamespace" -}}
{{- default .Release.Namespace .Values.controller.namespace }}
{{- end }}

{{- /*
  The selector holds only app.kubernetes.io/name, set to the full name. A Deployment selector
  cannot change, so an install that adopts an existing Deployment must render the same selector.
  With fullnameOverride the selector is predictable.
*/}}
{{- define "dvc.selectorLabels" -}}
app.kubernetes.io/name: {{ include "dvc.fullname" . }}
{{- end }}

{{- define "dvc.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "dvc.selectorLabels" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/component: controller
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}
