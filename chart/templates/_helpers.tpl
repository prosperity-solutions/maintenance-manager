{{/*
Every object is named maintenance-manager: the SecurityPolicies point at the
Service by that name, and one install per cluster is the rule.
*/}}
{{- define "mm.name" -}}maintenance-manager{{- end -}}

{{- define "mm.selectorLabels" -}}
app.kubernetes.io/name: maintenance-manager
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "mm.labels" -}}
{{ include "mm.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: maintenance-manager
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end -}}

{{- define "mm.image" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default (printf "v%s" .Chart.AppVersion) }}
{{- end -}}
{{- end -}}

{{- define "mm.editorSecret" -}}
{{- .Values.editor.existingSecret | default (include "mm.name" .) -}}
{{- end -}}
