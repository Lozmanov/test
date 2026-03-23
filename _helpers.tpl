{{/*
Expand the name of the chart.
*/}}
{{- define "ozone.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "ozone.fullname" -}}
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
{{- define "ozone.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "ozone.labels" -}}
helm.sh/chart: {{ include "ozone.chart" . }}
{{ include "ozone.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "ozone.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ozone.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Get namespace
*/}}
{{- define "ozone.namespace" -}}
{{- if .Values.global.namespaceOverride }}
{{- .Values.global.namespaceOverride }}
{{- else }}
{{- .Release.Namespace }}
{{- end }}
{{- end }}

{{/*
Get nodeName for StatefulSet pod by index
*/}}
{{- define "ozone.nodeName" -}}
{{- $component := .component -}}
{{- $index := int .index -}}
{{- $values := .values -}}
{{- if eq $component "om" -}}
  {{- index $values.om.nodeNames $index | default (printf "node-om-%d" $index) -}}
{{- else if eq $component "scm" -}}
  {{- index $values.scm.nodeNames $index | default (printf "node-scm-%d" $index) -}}
{{- else if eq $component "datanode" -}}
  {{- index $values.datanode.nodeNames $index | default (printf "node-dn-%d" $index) -}}
{{- else if eq $component "recon" -}}
  {{- index $values.recon.nodeNames 0 | default "node-recon-0" -}}
{{- end -}}
{{- end -}}

{{/*
Full FQDN for OM service
*/}}
{{- define "ozone.om.fqdn" -}}
{{ printf "%s.%s.svc.%s" .Values.om.serviceName (include "ozone.namespace" .) .Values.global.clusterDomain }}
{{- end -}}

{{/*
Full FQDN for SCM service
*/}}
{{- define "ozone.scm.fqdn" -}}
{{ printf "%s.%s.svc.%s" .Values.scm.serviceName (include "ozone.namespace" .) .Values.global.clusterDomain }}
{{- end -}}

{{/*
Full FQDN for Recon
*/}}
{{- define "ozone.recon.fqdn" -}}
{{ printf "%s.%s.svc.%s" .Values.recon.serviceName (include "ozone.namespace" .) .Values.global.clusterDomain }}
{{- end -}}

{{/*
Full FQDN for HUE service
*/}}
{{- define "ozone.hue.fqdn" -}}
{{ printf "%s.%s.svc.%s" .Values.hue.serviceName (include "ozone.namespace" .) .Values.global.clusterDomain }}
{{- end -}}

{{/*
Full FQDN for HiveMetastore service
*/}}
{{- define "ozone.hivemetastore.fqdn" -}}
{{ printf "%s.%s.svc.%s" .Values.hiveMetastore.serviceName (include "ozone.namespace" .) .Values.global.clusterDomain }}
{{- end -}}

{{/*
Returns the Istio ServiceEntry hostname for an external database host.
If the value is a bare IPv4 address (e.g. "10.0.0.5"), a synthetic DNS-safe
hostname is generated ("ext-db-10-0-0-5.mesh") because Istio does not allow
IP literals in ServiceEntry.hosts. For a regular hostname the value is returned
unchanged.
Usage: {{ include "ozone.externalDbHost" "10.0.0.5" }}
       {{ include "ozone.externalDbHost" "pg.example.com" }}
*/}}
{{- define "ozone.externalDbHost" -}}
{{- if regexMatch "^[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+$" . -}}
{{- printf "ext-db-%s.mesh" (. | replace "." "-") -}}
{{- else -}}
{{- . -}}
{{- end -}}
{{- end -}}

{{/*
Container-level securityContext from global settings.
Usage: {{- include "ozone.containerSecurityContext" $ | nindent 10 }}
*/}}
{{- define "ozone.containerSecurityContext" -}}
{{- with .Values.global.securityContext }}
securityContext:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}


{{/*
Istio sidecar injection annotation.
Outputs sidecar.istio.io/inject: "true" when istio.enabled and istio.injection.enabled are both true.
Usage: {{- include "ozone.istioAnnotations" . | nindent 8 }}
*/}}
{{- define "ozone.istioAnnotations" -}}
{{- if and .Values.istio.enabled .Values.istio.injection.enabled }}
sidecar.istio.io/inject: "true"
proxy.istio.io/config: '{"holdApplicationUntilProxyStarts": true}'
{{- end }}
{{- end -}}

