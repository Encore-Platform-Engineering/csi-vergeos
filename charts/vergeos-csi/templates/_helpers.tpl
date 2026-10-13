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

{{/* Private VergeOS root CA: inline caBundle xor existing caSecret. Renders "true" when set. */}}
{{- define "vergeos-csi.caEnabled" -}}
{{- if and .Values.vergeos.caBundle .Values.vergeos.caSecret -}}
{{- fail "set only one of vergeos.caBundle and vergeos.caSecret" -}}
{{- end -}}
{{- if or .Values.vergeos.caBundle .Values.vergeos.caSecret -}}true{{- end -}}
{{- end -}}

{{- define "vergeos-csi.caConfigMapName" -}}
{{- printf "%s-ca" (include "vergeos-csi.name" .) -}}
{{- end -}}

{{/* Pod-template annotation: rolls controller pods when the CA changes. */}}
{{- define "vergeos-csi.caAnnotations" -}}
{{- if eq (include "vergeos-csi.caEnabled" .) "true" -}}
checksum/vergeos-ca: {{ ternary .Values.vergeos.caBundle (printf "secret/%s/%s" .Values.vergeos.caSecret (.Values.vergeos.caSecretKey | default "ca.crt")) (not (empty .Values.vergeos.caBundle)) | sha256sum }}
{{- end -}}
{{- end -}}

{{/* Container env: SSL_CERT_FILE makes Go (govergeos) trust the mounted CA. */}}
{{- define "vergeos-csi.caEnv" -}}
{{- if eq (include "vergeos-csi.caEnabled" .) "true" -}}
env:
  - name: SSL_CERT_FILE
    value: /etc/vergeos/ca.crt
{{- end -}}
{{- end -}}

{{- define "vergeos-csi.caVolumeMount" -}}
{{- if eq (include "vergeos-csi.caEnabled" .) "true" -}}
- name: vergeos-ca
  mountPath: /etc/vergeos/ca.crt
  subPath: ca.crt
  readOnly: true
{{- end -}}
{{- end -}}

{{- define "vergeos-csi.caVolume" -}}
{{- if .Values.vergeos.caSecret -}}
- name: vergeos-ca
  secret:
    secretName: {{ .Values.vergeos.caSecret }}
    items:
      - key: {{ .Values.vergeos.caSecretKey | default "ca.crt" }}
        path: ca.crt
{{- else if .Values.vergeos.caBundle -}}
- name: vergeos-ca
  configMap:
    name: {{ include "vergeos-csi.caConfigMapName" . }}
    items:
      - key: ca.crt
        path: ca.crt
{{- end -}}
{{- end -}}
