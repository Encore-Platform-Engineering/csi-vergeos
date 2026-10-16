#!/usr/bin/env bash
# Helm-template assertions for the chart + raw manifests. Needs helm and yq v4.
# Optional: IMAGE_REF_TAG=<git tag> also asserts the chart image tag equals it.
set -euo pipefail
cd "$(dirname "$0")/.."

CHART=charts/vergeos-csi
REPO=ghcr.io/encore-platform-engineering/csi-vergeos
KDIR=/var/lib/k0s/kubelet
gq() { grep "$@" >/dev/null; }  # no -q: -q SIGPIPEs yq under pipefail
fail() { echo "FAIL: $*" >&2; exit 1; }
render() { helm template x "$CHART" --set block.poolVmId=1 "$@"; }
# q <file> <yq expr>: prints result; expr must yield non-null output.
q() { local f=$1; shift; yq eval -e "$@" "$f"; }

render --set kubeletDir=$KDIR > /tmp/chart-k0s.yaml
render > /tmp/chart-default.yaml
render --set block.enabled=false > /tmp/chart-nas-only.yaml

echo "== images"
if grep -rnE ':latest|vergeio|ghcr\.io/verge-io' "$CHART" deploy Makefile README.md; then
  fail "mutable or foreign image reference"
fi
appver=$(yq eval '.appVersion' $CHART/Chart.yaml)
# chart version == appVersion, so on a tag (IMAGE_REF_TAG) chart, image and tag all match
[ "$(yq eval '.version' $CHART/Chart.yaml)" = "$appver" ] || fail "Chart.yaml version != appVersion"
want="$REPO:v$appver"
[ -z "${IMAGE_REF_TAG:-}" ] || want="$REPO:$IMAGE_REF_TAG"
for f in /tmp/chart-default.yaml deploy/kubernetes/node.yaml deploy/kubernetes/block-controller.yaml deploy/kubernetes/nas-controller.yaml; do
  bad=$(yq eval '[.. | select(has("image")) | .image | select(test("csi-vergeos"))] | .[] | select(. != "'"$want"'")' "$f")
  [ -z "$bad" ] || fail "$f: image(s) '$bad' != $want"
done

echo "== node staging mount (k0s kubeletDir)"
node='select(.kind=="DaemonSet" and .metadata.name=="x-node")'
q /tmp/chart-k0s.yaml "$node | .spec.template.spec.volumes[] | select(.name==\"csi-staging-dir\" and .hostPath.path==\"$KDIR/plugins/kubernetes.io/csi\")" >/dev/null
q /tmp/chart-k0s.yaml "$node | .spec.template.spec.containers[] | select(.name==\"csi-vergeos-block\") | .volumeMounts[] | select(.mountPath==\"$KDIR/plugins/kubernetes.io/csi\" and .mountPropagation==\"Bidirectional\")" >/dev/null
q deploy/kubernetes/node.yaml '.spec.template.spec.containers[] | select(.name=="csi-vergeos-block") | .volumeMounts[] | select(.mountPath=="/var/lib/kubelet/plugins/kubernetes.io/csi" and .mountPropagation=="Bidirectional")' >/dev/null
# block disabled: no staging volume
if yq eval "$node | .spec.template.spec.volumes[].name" /tmp/chart-nas-only.yaml | gq csi-staging-dir; then
  fail "staging volume rendered with block.enabled=false"
fi

echo "== node RBAC / secrets"
for f in /tmp/chart-default.yaml deploy/kubernetes/rbac.yaml; do
  case $f in deploy*) ns=csi-vergeos-node ;; *) ns=x-node ;; esac
  q "$f" "select(.kind==\"ClusterRoleBinding\" and .subjects[0].name==\"$ns\") | .roleRef.name" | gq -x "$ns" || fail "$f: node binding"
  rules=$(yq eval -o=json -I0 "select(.kind==\"ClusterRole\" and .metadata.name==\"$ns\") | .rules" "$f")
  [ "$rules" = '[{"apiGroups":[""],"resources":["nodes"],"verbs":["get"]}]' ] || fail "$f: node ClusterRole is not get-nodes only: $rules"
  # nothing but the node ClusterRoleBinding may bind the node SA
  n=$(yq eval "select(.kind==\"ClusterRoleBinding\" or .kind==\"RoleBinding\") | select(.subjects[].name==\"$ns\") | .metadata.name" "$f" | grep -c .)
  [ "$n" = 1 ] || fail "$f: node SA has $n bindings"
  # leases are namespaced (Role), never in a ClusterRole
  if yq eval 'select(.kind=="ClusterRole") | .rules[].resources[]' "$f" | gq -x leases; then fail "$f: leases in ClusterRole"; fi
  q "$f" 'select(.kind=="Role") | .rules[].resources[]' | gq -x leases || fail "$f: no leases Role"
done
for f in /tmp/chart-default.yaml deploy/kubernetes/node.yaml; do
  if yq eval 'select(.kind=="DaemonSet") | .. | select(has("secretRef") or has("secretKeyRef") or has("secret"))' "$f" | gq .; then
    fail "$f: node pod references a Secret"
  fi
  q "$f" 'select(.kind=="DaemonSet") | .spec.template.spec.serviceAccountName' | gq -E '(^|-)node$' || fail "$f: node SA"
done
# NAS-only chart: controller must not get volumeattachments
if yq eval 'select(.kind=="ClusterRole") | .rules[].resources[]' /tmp/chart-nas-only.yaml | gq volumeattachments; then
  fail "volumeattachments rule rendered with block.enabled=false"
fi

echo "== tolerations / priority / liveness"
for f in /tmp/chart-default.yaml deploy/kubernetes/node.yaml; do
  q "$f" 'select(.kind=="DaemonSet") | .spec.template.spec | select(.priorityClassName=="system-node-critical") | .tolerations[] | select(.operator=="Exists")' >/dev/null || fail "$f: tolerations/priority"
done
for f in /tmp/chart-default.yaml deploy/kubernetes/node.yaml deploy/kubernetes/block-controller.yaml deploy/kubernetes/nas-controller.yaml; do
  # every csi-vergeos driver container has an httpGet /healthz probe on a declared port
  bad=$(yq eval 'select(.kind=="DaemonSet" or .kind=="Deployment") | .spec.template.spec.containers[] | select(.name | test("^csi-vergeos")) | . as $c | select(($c.livenessProbe.httpGet.path != "/healthz") or (([$c.ports[].name] | contains([$c.livenessProbe.httpGet.port])) | not)) | .name' "$f")
  [ -z "$bad" ] || fail "$f: driver container(s) without wired livenessProbe: $bad"
  # every livenessprobe sidecar exposes an http endpoint
  have=$(yq eval -N 'select(.kind=="DaemonSet" or .kind=="Deployment") | .spec.template.spec.containers[] | select(.name | test("^liveness-probe")) | .args[]' "$f" | grep -c -- '--http-endpoint=' || true)
  want_n=$(yq eval -N 'select(.kind=="DaemonSet" or .kind=="Deployment") | .spec.template.spec.containers[] | select(.name | test("^liveness-probe")) | .name' "$f" | grep -c .)
  [ "$have" = "$want_n" ] || fail "$f: livenessprobe sidecar without --http-endpoint"
done
echo "== NAS export ACL parameters"
nassc='select(.kind=="StorageClass" and .provisioner=="csi-nas.verge.io") | .parameters'
q /tmp/chart-default.yaml "$nassc | select(.allowedHosts != null and .allowAll == null)" >/dev/null || fail "default NAS StorageClass must set allowedHosts and not allowAll"
render --set nas.storageClass.allowAll=true > /tmp/chart-allowall.yaml
q /tmp/chart-allowall.yaml "$nassc | select(.allowAll == \"true\" and .allowedHosts == null)" >/dev/null || fail "allowAll=true must render allowAll and no allowedHosts"
q deploy/kubernetes/storageclass.yaml 'select(.provisioner=="csi-nas.verge.io") | .parameters | select(.allowedHosts != null)' >/dev/null || fail "deploy NAS StorageClass lacks allowedHosts"

echo "== private VergeOS CA (caBundle / caSecret)"
ctl='select(.kind=="Deployment" and (.metadata.name=="x-block-controller" or .metadata.name=="x-nas-controller"))'
# default: no CA wiring at all
if yq eval "$ctl | .. | select(has(\"name\")) | .name" /tmp/chart-default.yaml | gq -E '^(vergeos-ca|SSL_CERT_FILE)$'; then
  fail "CA volume/env rendered without caBundle/caSecret"
fi
if yq eval 'select(.kind=="ConfigMap") | .metadata.name' /tmp/chart-default.yaml | gq -x x-ca; then fail "CA ConfigMap rendered by default"; fi
render --set-file vergeos.caBundle=hack/testdata/ca.pem > /tmp/chart-cabundle.yaml
render --set vergeos.caSecret=my-ca --set vergeos.caSecretKey=root.pem > /tmp/chart-casecret.yaml
for f in /tmp/chart-cabundle.yaml /tmp/chart-casecret.yaml; do
  for d in x-block-controller x-nas-controller; do
    dep="select(.kind==\"Deployment\" and .metadata.name==\"$d\")"
    c="$dep | .spec.template.spec.containers[] | select(.name==\"csi-vergeos\")"
    q $f "$c | .env[] | select(.name==\"SSL_CERT_FILE\" and .value==\"/etc/vergeos/ca.crt\")" >/dev/null || fail "$f $d: SSL_CERT_FILE"
    q $f "$c | .volumeMounts[] | select(.name==\"vergeos-ca\" and .mountPath==\"/etc/vergeos/ca.crt\" and .readOnly==true)" >/dev/null || fail "$f $d: CA mount"
    q $f "$dep | .spec.template.metadata.annotations.\"checksum/vergeos-ca\"" >/dev/null || fail "$f $d: checksum annotation"
    # credentials envFrom is kept
    q $f "$c | .envFrom[].secretRef.name" >/dev/null || fail "$f $d: envFrom lost"
  done
done
for d in x-block-controller x-nas-controller; do
  q /tmp/chart-cabundle.yaml "select(.kind==\"Deployment\" and .metadata.name==\"$d\") | .spec.template.spec.volumes[] | select(.name==\"vergeos-ca\" and .configMap.name==\"x-ca\")" >/dev/null || fail "$d: caBundle volume"
  q /tmp/chart-casecret.yaml "select(.kind==\"Deployment\" and .metadata.name==\"$d\") | .spec.template.spec.volumes[] | select(.name==\"vergeos-ca\" and .secret.secretName==\"my-ca\" and .secret.items[0].key==\"root.pem\" and .secret.items[0].path==\"ca.crt\")" >/dev/null || fail "$d: caSecret volume"
done
q /tmp/chart-cabundle.yaml 'select(.kind=="ConfigMap" and .metadata.name=="x-ca") | .data."ca.crt"' | gq 'BEGIN CERTIFICATE' || fail "caBundle not rendered into ConfigMap x-ca"
if yq eval 'select(.kind=="ConfigMap") | .metadata.name' /tmp/chart-casecret.yaml | gq -x x-ca; then fail "caSecret must not render a CA ConfigMap"; fi
# a changed caBundle changes the checksum (rolls pods)
render --set-file vergeos.caBundle=hack/testdata/ca2.pem > /tmp/chart-cabundle2.yaml
a=$(yq eval 'select(.metadata.name=="x-block-controller") | .spec.template.metadata.annotations."checksum/vergeos-ca"' /tmp/chart-cabundle.yaml)
b=$(yq eval 'select(.metadata.name=="x-block-controller") | .spec.template.metadata.annotations."checksum/vergeos-ca"' /tmp/chart-cabundle2.yaml)
[ -n "$a" ] && [ "$a" != "$b" ] || fail "checksum/vergeos-ca did not change with caBundle"
# mutually exclusive; block-disabled still renders for NAS
if render --set vergeos.caBundle=x --set vergeos.caSecret=y >/dev/null 2>&1; then fail "caBundle + caSecret must be rejected"; fi
render --set block.enabled=false --set vergeos.caSecret=my-ca | yq eval 'select(.metadata.name=="x-nas-controller") | .spec.template.spec.volumes[].name' - | gq -x vergeos-ca || fail "NAS-only CA volume"

echo "== block StorageClass default annotation"
blocksc='select(.kind=="StorageClass" and .provisioner=="csi-block.verge.io")'
if yq eval "$blocksc | .metadata.annotations" /tmp/chart-default.yaml | gq is-default-class; then fail "block StorageClass is default without isDefault"; fi
render --set block.storageClass.isDefault=true | yq eval "$blocksc | .metadata.annotations.\"storageclass.kubernetes.io/is-default-class\"" - | gq -x true || fail "isDefault=true must annotate the block StorageClass"

echo OK
