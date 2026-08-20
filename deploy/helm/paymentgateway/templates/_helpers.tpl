{{/*
Helpers. Most templates receive a context dict: (dict "root" $ "name" <service-name> "svc" <merged service values>)
*/}}

{{- define "pg.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Release-wide name prefix (mirrors the common chart "fullname" convention). Takes root context. */}}
{{- define "pg.prefix" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 40 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 40 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 40 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Full resource name for a service: <prefix>-<service>. Takes ctx dict. */}}
{{- define "pg.fullname" -}}
{{- printf "%s-%s" (include "pg.prefix" .root) .name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Merge serviceDefaults with a service entry. Usage: $svc := include "pg.mergeSvc" (dict "root" $ "raw" $raw) | fromYaml */}}
{{- define "pg.mergeSvc" -}}
{{- mergeOverwrite (deepCopy .root.Values.serviceDefaults) (default (dict) .raw) | toYaml -}}
{{- end -}}

{{/* Short name used for migrations/<short> (name minus "-service" unless overridden). */}}
{{- define "pg.shortName" -}}
{{- if .svc.migrationsName -}}{{ .svc.migrationsName }}{{- else -}}{{ .name | trimSuffix "-service" }}{{- end -}}
{{- end -}}

{{- define "pg.imageTag" -}}
{{- .svc.image.tag | default .root.Values.global.imageTag | default .root.Chart.AppVersion -}}
{{- end -}}

{{- define "pg.image" -}}
{{- $repo := .svc.image.repository | default (printf "paymentgateway/%s" .name) -}}
{{- $reg := .root.Values.global.imageRegistry -}}
{{- if $reg -}}{{ printf "%s/%s:%s" $reg $repo (include "pg.imageTag" .) }}{{- else -}}{{ printf "%s:%s" $repo (include "pg.imageTag" .) }}{{- end -}}
{{- end -}}

{{- define "pg.selectorLabels" -}}
app.kubernetes.io/name: {{ .name }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- end -}}

{{- define "pg.labels" -}}
helm.sh/chart: {{ include "pg.chart" .root }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
app.kubernetes.io/part-of: paymentgateway
app.kubernetes.io/component: {{ .name }}
app.kubernetes.io/version: {{ include "pg.imageTag" . | quote }}
{{ include "pg.selectorLabels" . }}
{{- end -}}

{{/* Labels shared by every pod of the release (used by NetworkPolicy selectors). Takes root. */}}
{{- define "pg.partOfSelector" -}}
app.kubernetes.io/part-of: paymentgateway
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "pg.serviceAccountName" -}}
{{- if .svc.serviceAccount.create -}}
{{- .svc.serviceAccount.name | default (include "pg.fullname" .) -}}
{{- else -}}
{{- .svc.serviceAccount.name | default "default" -}}
{{- end -}}
{{- end -}}

{{/* In-cluster gRPC address of a service: <fullname>.<ns>.svc.cluster.local:<grpcPort> */}}
{{- define "pg.grpcAddr" -}}
{{- printf "%s.%s.svc.cluster.local:%v" (include "pg.fullname" .) .root.Release.Namespace .svc.grpcPort -}}
{{- end -}}

{{/* PG_<SERVICE>_ADDR entries for every enabled gRPC service that is not a provider adapter. Takes root. */}}
{{- define "pg.serviceAddrEnv" -}}
{{- $root := . -}}
{{- range $n, $raw := $root.Values.services -}}
{{- $s := include "pg.mergeSvc" (dict "root" $root "raw" $raw) | fromYaml -}}
{{- if and $s.enabled $s.grpcPort (not $s.providerKey) }}
{{ printf "PG_%s_ADDR" ($n | replace "-" "_" | upper) }}: {{ include "pg.grpcAddr" (dict "root" $root "name" $n "svc" $s) | quote }}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* PG_PROVIDER_ADDRS value: key=addr,key=addr for every enabled provider adapter. Takes root. */}}
{{- define "pg.providerAddrs" -}}
{{- $root := . -}}
{{- $list := list -}}
{{- range $n, $raw := $root.Values.services -}}
{{- $s := include "pg.mergeSvc" (dict "root" $root "raw" $raw) | fromYaml -}}
{{- if and $s.enabled $s.providerKey -}}
{{- $list = append $list (printf "%s=%s" $s.providerKey (include "pg.grpcAddr" (dict "root" $root "name" $n "svc" $s))) -}}
{{- end -}}
{{- end -}}
{{- join "," $list -}}
{{- end -}}

{{/* Name of the Secret produced by the ExternalSecret of a service. */}}
{{- define "pg.secretName" -}}
{{- printf "%s-env" (include "pg.fullname" .) -}}
{{- end -}}

{{/* Whether a service consumes an ExternalSecret. */}}
{{- define "pg.hasSecret" -}}
{{- if and .root.Values.externalSecrets.enabled .svc.externalSecret.enabled -}}true{{- end -}}
{{- end -}}

{{/* Checksum that changes whenever this service's configuration inputs change (forces a rollout). */}}
{{- define "pg.configChecksum" -}}
{{- toJson (dict "global" .root.Values.global "svc" .svc "svcs" (keys .root.Values.services)) | sha256sum -}}
{{- end -}}
