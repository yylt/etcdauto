{{/*
Expand the name of the chart.
*/}}
{{- define "etcdauto.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "etcdauto.fullname" -}}
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
Create chart name and version as used by the chart label.
*/}}
{{- define "etcdauto.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "etcdauto.labels" -}}
helm.sh/chart: {{ include "etcdauto.chart" . }}
{{ include "etcdauto.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "etcdauto.selectorLabels" -}}
app.kubernetes.io/name: {{ include "etcdauto.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ECSNode service account name
*/}}
{{- define "etcdauto.ecsnode.serviceAccountName" -}}
{{- if .Values.ecsnode.serviceAccount.create }}
{{- default "ecsnode" .Values.ecsnode.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.ecsnode.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
etcd service account name
*/}}
{{- define "etcdauto.etcd.serviceAccountName" -}}
{{- if .Values.etcd.serviceAccount.create }}
{{- default "etcdcluster" .Values.etcd.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.etcd.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Namespace
*/}}
{{- define "etcdauto.namespace" -}}
{{- default .Release.Namespace .Values.global.namespace }}
{{- end }}
