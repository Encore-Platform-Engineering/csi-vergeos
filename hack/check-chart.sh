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
echo OK
