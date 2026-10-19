{{- define "cg.name" -}}{{ .Release.Name }}-changegate-executor{{- end -}}
{{- define "cg.labels" -}}
app.kubernetes.io/name: changegate-executor
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}
