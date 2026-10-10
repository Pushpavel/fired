{{- define "fired.labels" -}}
app.kubernetes.io/part-of: fired
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/* Postgres URL fired uses: the in-chart StatefulSet, or externalURL. */}}
{{- define "fired.dbURL" -}}
{{- if .Values.postgres.enabled -}}
postgres://fired:{{ required "postgres.password is required" .Values.postgres.password | urlquery }}@fired-db.{{ .Release.Namespace }}.svc:5432/fired?sslmode=disable
{{- else -}}
{{ required "postgres.externalURL is required when postgres.enabled is false" .Values.postgres.externalURL }}
{{- end -}}
{{- end }}

{{/* fired's config.yaml for one group: chart settings < values config < group config. */}}
{{- define "fired.config" -}}
{{- $base := dict "listen" ":8443" "driver" "firecracker" -}}
{{- $cfg := mustMergeOverwrite $base (deepCopy (.root.Values.fired.config | default dict)) (deepCopy (.group.config | default dict)) -}}
{{- toYaml $cfg }}
{{- end }}
