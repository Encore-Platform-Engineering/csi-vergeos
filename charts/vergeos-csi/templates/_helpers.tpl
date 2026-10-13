{{- define "vergeos-csi.name" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "vergeos-csi.secretName" -}}
{{- if .Values.vergeos.existingSecret -}}
{{- .Values.vergeos.existingSecret -}}
{{- else -}}
{{- include "vergeos-csi.name" . -}}-credentials
{{- end -}}
{{- end -}}

{{- define "vergeos-csi.controllerName" -}}
{{- printf "%s-controller" (include "vergeos-csi.name" .) -}}
{{- end -}}

{{- define "vergeos-csi.nodeName" -}}
{{- printf "%s-node" (include "vergeos-csi.name" .) -}}
{{- end -}}

{{/* Release images are tagged v<semver> by CI; default tag = v<appVersion>. */}}
{{- define "vergeos-csi.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default (printf "v%s" .Chart.AppVersion) }}
{{- end -}}

{{/* block.poolVmId: must be set to a real VM $key (not empty/0). */}}
{{- define "vergeos-csi.poolVmId" -}}
{{- $id := required "block.poolVmId is required when block.enabled=true (set --set block.poolVmId=<VM $key>, or block.enabled=false)" .Values.block.poolVmId | toString -}}
{{- if eq $id "0" -}}{{- fail "block.poolVmId must not be 0: set it to the pool VM $key, or block.enabled=false" -}}{{- end -}}
{{- $id -}}
{{- end }}
