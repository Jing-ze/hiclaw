{{/*
Infrastructure abstraction helpers.

Phase 1 keeps reading legacy values paths (`matrixServer.*`, `aiGateway.*`,
`objectStorage.*`, `elementWeb.matrixServerUrl`) so templates can decouple
from provider-specific structures before the public values API is renamed.
*/}}

{{- define "hiclaw.matrix.internalURL" -}}
{{- if eq .Values.matrixServer.type "tuwunel" -}}
{{- include "hiclaw.tuwunel.internalURL" . -}}
{{- else -}}
{{- .Values.matrixServer.externalURL | default "" -}}
{{- end -}}
{{- end }}

{{- define "hiclaw.matrix.serverName" -}}
{{- if eq .Values.matrixServer.type "tuwunel" -}}
{{- include "hiclaw.tuwunel.serverName" . -}}
{{- else -}}
{{- .Values.matrixServer.serverName | default "" -}}
{{- end -}}
{{- end }}

{{- define "hiclaw.gateway.publicURL" -}}
{{- default (include "hiclaw.matrix.internalURL" .) .Values.elementWeb.matrixServerUrl -}}
{{- end }}

{{- define "hiclaw.gateway.internalURL" -}}
{{- if eq .Values.aiGateway.type "higress" -}}
{{- include "hiclaw.higress.gatewayURL" . -}}
{{- else -}}
{{- .Values.aiGateway.external.gatewayUrl | default "" -}}
{{- end -}}
{{- end }}

{{- define "hiclaw.gateway.adminURL" -}}
{{- if eq .Values.aiGateway.type "higress" -}}
{{- include "hiclaw.higress.consoleURL" . -}}
{{- else -}}
{{- .Values.aiGateway.external.consoleUrl | default "" -}}
{{- end -}}
{{- end }}

{{- define "hiclaw.gateway.higress.enabled" -}}
{{- if eq .Values.aiGateway.type "higress" -}}true{{- else -}}false{{- end -}}
{{- end }}

{{- define "hiclaw.gateway.adminSecretName" -}}
{{- if eq .Values.aiGateway.type "higress" -}}higress-console{{- end -}}
{{- end }}

{{- define "hiclaw.gateway.adminPasswordKey" -}}
{{- if eq .Values.aiGateway.type "higress" -}}adminPassword{{- end -}}
{{- end }}

{{- define "hiclaw.storage.endpoint" -}}
{{- if eq .Values.objectStorage.type "minio" -}}
{{- include "hiclaw.minio.internalURL" . -}}
{{- else -}}
{{- .Values.objectStorage.external.endpoint | default "" -}}
{{- end -}}
{{- end }}

{{- define "hiclaw.storage.bucket" -}}
{{- if eq .Values.objectStorage.type "minio" -}}
{{- .Values.objectStorage.minio.bucketName -}}
{{- else -}}
{{- .Values.objectStorage.external.bucket | default "" -}}
{{- end -}}
{{- end }}

{{- define "hiclaw.storage.remoteRoot" -}}
{{- printf "hiclaw/%s" (include "hiclaw.storage.bucket" .) -}}
{{- end }}

{{- define "hiclaw.storage.adminSecretName" -}}
{{- if eq .Values.objectStorage.type "minio" -}}
{{- include "hiclaw.minio.fullname" . -}}
{{- end -}}
{{- end }}

{{- define "hiclaw.storage.adminAccessKeyKey" -}}
{{- if eq .Values.objectStorage.type "minio" -}}MINIO_ROOT_USER{{- end -}}
{{- end }}

{{- define "hiclaw.storage.adminSecretKeyKey" -}}
{{- if eq .Values.objectStorage.type "minio" -}}MINIO_ROOT_PASSWORD{{- end -}}
{{- end }}

{{- define "hiclaw.manager.spec" -}}
{{- $spec := dict
  "model" (.Values.manager.model | default .Values.credentials.defaultModel)
  "runtime" (.Values.manager.runtime | default "openclaw")
  "image" (include "hiclaw.manager.image" .)
  "resources" .Values.manager.resources
-}}
{{- $spec | toJson -}}
{{- end }}
