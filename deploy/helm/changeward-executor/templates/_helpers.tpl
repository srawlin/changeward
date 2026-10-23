{{- define "cw.name" -}}{{ .Release.Name }}-changeward-executor{{- end -}}
{{- define "cw.labels" -}}
app.kubernetes.io/name: changeward-executor
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}

{{- define "cw.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}{{ default (include "cw.name" .) .Values.serviceAccount.name }}{{- else -}}{{ default "default" .Values.serviceAccount.name }}{{- end -}}
{{- end }}
