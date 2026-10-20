{{/* chart 名称。 */}}
{{- define "mars-agent.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* 资源全名：Release 名已包含 chart 名时不再重复拼接。 */}}
{{- define "mars-agent.fullname" -}}
{{- if contains .Chart.Name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/* chart 版本标签。 */}}
{{- define "mars-agent.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* 通用标签：所有资源共用。 */}}
{{- define "mars-agent.labels" -}}
helm.sh/chart: {{ include "mars-agent.chart" . }}
{{ include "mars-agent.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/* 选择器标签：一经发布不可变更，只放稳定标识。 */}}
{{- define "mars-agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "mars-agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/* 容器端口：探针、MARS_LISTEN_ADDR 与 NOTES 的 port-forward 共用同一来源，避免端口漂移。 */}}
{{- define "mars-agent.containerPort" -}}
{{- 4096 }}
{{- end }}

{{/* ServiceAccount 名：固定使用资源全名。 */}}
{{- define "mars-agent.serviceAccountName" -}}
{{- include "mars-agent.fullname" . }}
{{- end }}
