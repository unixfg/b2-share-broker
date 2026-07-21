{{/*
Common name helpers.
*/}}
{{- define "b2-share-broker.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Short app prefix for component resources (processor, staging).
The chart is named b2-share-broker; component resources use b2-share-*
to match the pre-Helm kustomize names.
*/}}
{{- define "b2-share-broker.shortName" -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if hasSuffix "-broker" $name -}}{{- trimSuffix "-broker" $name -}}{{- else -}}{{- $name -}}{{- end -}}
{{- end -}}

{{/*
Fully qualified app name.
*/}}
{{- define "b2-share-broker.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Common labels.
*/}}
{{- define "b2-share-broker.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
Image reference with optional digest pin.
*/}}
{{- define "b2-share-broker.image" -}}
{{- $repo := .Values.image.repository -}}
{{- $tag := .Values.image.tag -}}
{{- if .Values.image.digest -}}
{{- printf "%s:%s@%s" $repo $tag .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repo $tag -}}
{{- end -}}
{{- end -}}

{{/*
Namespace for chart resources.
*/}}
{{- define "b2-share-broker.namespace" -}}
{{- if .Values.namespace.create -}}
{{- default .Release.Namespace .Values.namespace.name -}}
{{- else -}}
{{- default .Release.Namespace .Values.namespace.name -}}
{{- end -}}
{{- end -}}
