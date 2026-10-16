# CSI Driver for VergeOS

[![Go 1.25+](https://img.shields.io/badge/go-1.25+-00ADD8.svg?logo=go&logoColor=white)](https://go.dev/dl/)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-v1.32+-326CE5.svg?logo=kubernetes&logoColor=white)](https://kubernetes.io/)
[![CSI Spec](https://img.shields.io/badge/CSI-v1.12.0-8C4FFF.svg)](https://github.com/container-storage-interface/spec)

Kubernetes [Container Storage Interface](https://github.com/container-storage-interface/spec) (CSI) driver that provisions persistent volumes directly on the VergeOS vSAN. A single Go binary serves two CSI drivers, selected by flags at startup:

| Driver | Name | Access Mode | Description |
|--------|------|-------------|-------------|
| **NAS** | `csi-nas.verge.io` | ReadWriteMany | EXT4 volumes on VergeOS NAS services, exposed over NFS |
| **Block** | `csi-block.verge.io` | ReadWriteOnce | VM drives hotplugged to VergeOS VMs via the vSAN |

## Why Not Longhorn?

Longhorn runs its own replicated storage engine *inside* Kubernetes, layering replication, snapshots, and scheduling on top of whatever the hypervisor already provides. On VergeOS this creates a redundant storage stack — Longhorn replicates data that the vSAN is already replicating, deduplicating, and tiering across nodes. The result is double the write amplification, wasted disk capacity, and an extra failure domain to manage.

This CSI driver delegates storage operations directly to the VergeOS API, letting the vSAN handle what it was built for:

- **No double replication** — the vSAN's distributed mirror architecture provides data redundancy; no need for Longhorn to replicate on top of it.
- **Inline deduplication** — vSAN dedup is global across the cluster. Longhorn volumes are opaque blobs that can't participate.
- **Multi-tier placement** — volumes land on the correct vSAN tier (NVMe, SSD, HDD) based on the StorageClass. Longhorn has no visibility into the underlying storage tiers.
- **Unified management** — volumes appear in the VergeOS UI alongside VMs, snapshots, and NAS shares. One operational surface instead of two.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  Kubernetes Cluster (VergeOS VMs)                           │
│                                                             │
│  ┌──────────────────┐  ┌──────────────────┐                 │
│  │  NAS Controller   │  │ Block Controller  │  Deployments   │
│  │  Deployment       │  │ Deployment        │                │
│  │  + provisioner    │  │ + provisioner     │                │
│  │    sidecar        │  │ + attacher sidecar│                │
│  └────────┬─────────┘  └────────┬──────────┘                │
│           │                     │                            │
│  ┌────────┴─────────────────────┴──────────┐                │
│  │         Node DaemonSet                   │  DaemonSet     │
│  │  (both NAS + Block node plugins)         │                │
│  │  + node-driver-registrar sidecars        │                │
│  └────────┬─────────────────────┬──────────┘                │
│           │                     │                            │
└───────────┼─────────────────────┼────────────────────────────┘
            │  VergeOS API        │
            ▼                     ▼
┌───────────────────┐  ┌──────────────────────┐
│  NAS Service      │  │  vSAN VM Drives      │
│  (EXT4 + NFS)     │  │  (hotplug to node)   │
└───────────────────┘  └──────────────────────┘
```

### NAS Driver Flow

1. **CreateVolume** — creates a NAS volume + NFS share via the VergeOS API.
2. **NodePublishVolume** — NFS-mounts the share into the pod's target path.
3. No controller attach step (`attachRequired: false`).

### Block Driver Flow

The block driver uses a **pool VM** — a dedicated VergeOS VM that acts as a holding area for drives when they're not attached to a Kubernetes node. The pool VM doesn't need to be running; it's just an owner for idle drives. Create any VM in VergeOS for this purpose and pass its ID as `--pool-vm-id` (or `block.poolVmId` in Helm).

1. **CreateVolume** — creates a VM drive on the pool VM with a deterministic serial number.
2. **ControllerPublishVolume** — moves the drive from the pool VM to the target node's VM and hotplugs it. The VM is found from the Node's `spec.providerID`, not its name: the node plugin reads its own Node (needs `get nodes`) and reports `uuid:<uuid>` for `vergeos://<uuid>` (Cluster API) or `key:<n>` for legacy `vergeos://<n>`; with no providerID yet it reports `name:<node name>` (with a warning) unless `--require-provider-id` is set. Lookups skip snapshot VMs and fail if more than one VM matches. Bare node names from older installs still resolve by VM name.
3. **NodeStageVolume** — discovers the device by serial in `/dev/disk/by-id/`, formats (EXT4), and mounts to a staging path.
4. **NodePublishVolume** — bind-mounts the staging path into the pod.
5. **ControllerUnpublishVolume** — resolves the node ID the same way, hot-unplugs the drive and moves it back to the pool VM. If the drive is held by a different (live) VM the call is a no-op; if the holder VM no longer exists the drive is moved straight back to the pool. A failed hot-unplug returns a retryable error and the drive is not moved.

Block volumes are ReadWriteOnce, `SINGLE_NODE_*` only; RWX and multi-node modes are rejected with `InvalidArgument`. Both filesystem and raw block (`volumeMode: Block`) are accepted; read-only raw block is rejected (see [Node volume capabilities](#node-volume-capabilities)). Only drives named `csi-*` are published, unpublished or deleted. Publishing a drive that is attached to another node fails with `FailedPrecondition` until it is unpublished there; a retry after a failed hotplug re-hotplugs. `DeleteVolume` refuses attached drives (`FailedPrecondition`). `CreateVolume` is idempotent by name on the pool VM only (no scan of other VMs): same name with an incompatible size returns `AlreadyExists`, `limit_bytes` is honored (`OutOfRange` if `required_bytes` exceeds it), and VergeOS list errors return `Unavailable` rather than risking a duplicate drive.

## Project Structure

```
cmd/csi-vergeos/main.go       Entry point, flag parsing, backend wiring
pkg/driver/                    gRPC server, Identity/Controller/Node dispatchers
pkg/driver/interfaces.go       ControllerBackend and NodeBackend interfaces
pkg/nas/                       NAS controller + node implementations
pkg/block/                     Block controller + node implementations
pkg/util/                      Shared mount and block-device utilities
charts/vergeos-csi/            Helm chart
deploy/kubernetes/             Raw K8s manifests (alternative to Helm)
```

## Installation

### Prerequisites

1. **Download kubeconfig from Rancher** — Cluster Management > *your cluster* > ⋮ > Download KubeConfig. Save and set:
   ```bash
   export KUBECONFIG=~/Downloads/<cluster>-kubeconfig.yaml
   kubectl get nodes  # verify access
   ```

2. **Create a pool VM in VergeOS** (for block storage) — Create an empty VM named `k8spool`. It never needs to boot; it just holds idle block drives. Look up its ID:
   ```bash
   curl -sk -H "x-yottabyte-token: <API_KEY>" 'https://<VERGEOS_HOST>/api/v4/vms?fields=name,$key' | grep k8spool
   # Returns: {"name":"k8spool","$key":65}
   ```

### Helm (recommended)

```bash
helm repo add verge-io https://verge-io.github.io/helm-charts
helm repo update

helm install vergeos-csi verge-io/vergeos-csi \
  --namespace kube-system \
  --set vergeos.host=https://<VERGEOS_HOST> \
  --set vergeos.apiKey=<API_KEY> \
  --set block.poolVmId=<POOL_VM_ID>
```

### Install from OCI

Each `v*` tag publishes the chart (version = tag without `v`) to `oci://ghcr.io/encore-platform-engineering/charts`. The package is private, so log in first (a token with `read:packages`):

```bash
helm registry login ghcr.io -u <GITHUB_USER>
helm install vergeos-csi oci://ghcr.io/encore-platform-engineering/charts/vergeos-csi \
  --version 0.1.1 --namespace kube-system \
  --set vergeos.host=https://<VERGEOS_HOST> \
  --set vergeos.apiKey=<API_KEY> \
  --set block.poolVmId=<POOL_VM_ID>
```

`helm template` takes the same arguments.

To install only the NAS driver (skip block):

```bash
helm install vergeos-csi verge-io/vergeos-csi \
  --namespace kube-system \
  --set vergeos.host=https://<VERGEOS_HOST> \
  --set vergeos.apiKey=<API_KEY> \
  --set block.enabled=false
```

On **k0s**, kubelet uses `/var/lib/k0s/kubelet`; add `--set kubeletDir=/var/lib/k0s/kubelet`. The value is used for the plugin socket, registration dir, the `--kubelet-registration-path`, and the Bidirectional pods and `plugins/kubernetes.io/csi` (block staging) mounts. A mismatch means the node plugin never registers.

Verify:

```bash
kubectl -n kube-system get pods | grep csi
kubectl get csidrivers
kubectl get storageclasses
```

Key Helm values:

| Value | Default | Description |
|-------|---------|-------------|
| `vergeos.host` | | VergeOS API URL (include `https://`) |
| `vergeos.apiKey` | | VergeOS API key |
| `vergeos.existingSecret` | | Use a pre-created Secret instead |
| `vergeos.verifySSL` | `"true"` | `"false"` disables TLS verification. Prefer `caBundle`/`caSecret` for a private CA |
| `vergeos.caBundle` | | Inline PEM of the private VergeOS root CA. Mutually exclusive with `caSecret` |
| `vergeos.caSecret` / `vergeos.caSecretKey` | | Existing Secret (same namespace) and key (default `ca.crt`) holding the CA PEM |
| `nas.enabled` | `true` | Deploy the NAS driver |
| `block.enabled` | `true` | Deploy the Block driver |
| `block.poolVmId` | | **Required** when block is enabled. VergeOS VM ID to hold idle block drives |
| `block.storageClass.isDefault` | `false` | Annotate the block StorageClass as the cluster default (`storageclass.kubernetes.io/is-default-class`) |
| `nas.storageClass.nasServiceName` | `k8s-nas` | VergeOS NAS service name |
| `nas.storageClass.allowedHosts` | RFC1918 ranges | IPs/CIDRs allowed to mount NFS exports. **Narrow to your node subnets** |
| `nas.storageClass.allowAll` | `false` | Export to every host (insecure; excludes `allowedHosts`) |
| `nas.storageClass.squash` | `no_root_squash` | `root_squash`, `all_squash` or `no_root_squash` |
| `kubeletDir` | `/var/lib/kubelet` | Kubelet root dir on nodes. Set to `/var/lib/k0s/kubelet` on k0s |
| `image.tag` | `v<appVersion>` | Image tag. Empty = `v` + chart `appVersion` (matches the `v*` tags CI pushes) |
| `node.tolerations` | `[{operator: Exists}]` | Node DaemonSet tolerations (runs on tainted nodes) |
| `node.priorityClassName` | `system-node-critical` | Node DaemonSet priority class |
| `controller.tolerations` / `controller.priorityClassName` | empty | Controller Deployment scheduling |
| `logLevel` | `5` | klog verbosity (0–10) |

### NAS export security

Each PV gets its own NFS share. StorageClass parameters control who may mount it:

| Parameter | Default | Meaning |
|-----------|---------|---------|
| `allowedHosts` | **required** | Comma-separated IPs/CIDRs (your node subnets) that may mount the export |
| `allowAll` | `false` | `"true"` exports to every host. Mutually exclusive with `allowedHosts` |
| `squash` | `root_squash` | `root_squash`, `all_squash` or `no_root_squash` |

`CreateVolume` fails with `InvalidArgument` if neither `allowedHosts` nor `allowAll: "true"` is set, or if `allowedHosts` contains `0.0.0.0/0` (use `allowAll` for that). Existing shares are never modified.

**Upgrading:** StorageClass `parameters` are immutable, so `helm upgrade` cannot add them to an existing `vergeos-nas` StorageClass. Delete and recreate it (`kubectl delete sc vergeos-nas`, then `helm upgrade`); existing PVs are untouched. Old StorageClasses without `allowedHosts` now fail provisioning.

Default decision: the driver is fail-closed with `root_squash`. The Helm chart and `deploy/` StorageClass ship `allowedHosts` = RFC1918 private ranges and `squash: no_root_squash` so NAS works out of the box: fresh exports are root-owned and `fsGroup` does not apply to NFS, so root-squashed pods could not write. Narrow `allowedHosts` to your node subnets for production. Mount options from the StorageClass `mountOptions` (e.g. `nfsvers=4.1`) are passed to `mount`; `nolock` is only added when you do not choose NFSv4 or set `lock`/`nolock` yourself.

### NAS service auto-creation

`nasServiceVM` creates a NAS service on that VM if `nasServiceName` does not exist. The VergeOS API accepts a service `name`, but the bundled govergeos client cannot set it, so VergeOS derives it from the VM name. Name the VM exactly `nasServiceName`; otherwise `CreateVolume` fails with `FailedPrecondition` (and never creates a second service on that VM). Pre-creating the NAS service is recommended.

### Private VergeOS CA

Set one of `vergeos.caBundle` (inline PEM) or `vergeos.caSecret` (+ `caSecretKey`), and keep `vergeos.verifySSL: "true"`. The chart mounts the CA at `/etc/vergeos/ca.crt` in both controller Deployments and sets `SSL_CERT_FILE` (Go replaces its default trust roots with that file, so it must contain the CA that signs the VergeOS cert). A changed `caBundle` rolls the pods; a changed `caSecret` Secret does not, so run `kubectl rollout restart` after editing it. Only the controllers talk to VergeOS; the node plugin needs no CA.

```bash
helm upgrade --install csi charts/vergeos-csi ... --set-file vergeos.caBundle=vergeos-root.pem
```

### Node volume capabilities

Block volumes support `fsType` `ext4` (default) and `xfs`; other types are rejected with `InvalidArgument`. StorageClass `mountOptions` are passed to the filesystem mount. `volumeMode: Block` is supported on the block driver with `ReadWriteOnce` (the device node is bind-mounted into the pod, never formatted) and rejected by the NAS driver. Read-only raw block (`SINGLE_NODE_READER_ONLY`, `readOnly: true`) is rejected: a read-only bind mount does not stop writes to a device node.

### Raw manifests

The manifests in `deploy/kubernetes/` are **default-kubelet-only** (`/var/lib/kubelet`). They are not templated. For k0s or any other kubelet dir, use the Helm chart (`kubeletDir`) or generate manifests with `helm template --set kubeletDir=<dir> charts/vergeos-csi`.

Images are pinned to `ghcr.io/encore-platform-engineering/csi-vergeos:v0.1.1`. The node DaemonSet has its own ServiceAccount (`get nodes` only) and does not receive the VergeOS credentials Secret; only the controllers do.

Apply the manifests in `deploy/kubernetes/` directly. You'll need to create a `vergeos-credentials` Secret manually:

```bash
kubectl -n kube-system create secret generic vergeos-credentials \
  --from-literal=VERGEOS_HOST=https://<HOST> \
  --from-literal=VERGEOS_API_KEY=<API_KEY> \
  --from-literal=VERGEOS_VERIFY_SSL=true   # set to "false" for self-signed certs
kubectl apply -f deploy/kubernetes/
```

## Building

```bash
make build            # Local binary → bin/csi-vergeos
make build-linux      # Cross-compile for Linux amd64
make docker-build     # Container image (linux/amd64, local)
make docker-push      # Build and push to ghcr.io/encore-platform-engineering/csi-vergeos
make test             # All tests with -race
go vet ./...          # Static analysis
hack/check-chart.sh   # helm template / raw manifest assertions (needs helm, yq)
```

CI (`.github/workflows/ci.yml`) runs `go vet`, `go test`, `hack/check-chart.sh` and a `docker build` on every PR. Pushing a `v*` tag also pushes `ghcr.io/encore-platform-engineering/csi-vergeos:<tag>`; the chart default image tag is `v<appVersion>`, so bump `appVersion` and the raw manifests' tag together with the release tag.

## Configuration

The binary takes these flags:

| Flag | Values | Description |
|------|--------|-------------|
| `--driver` | `nas` / `block` | Which CSI driver name to register |
| `--mode` | `controller` / `node` | Which gRPC services to run |
| `--endpoint` | `unix:///csi/csi.sock` | gRPC listen address |
| `--node-id` | hostname | Kubernetes Node name (node mode only). The block driver reports its CSI node ID from the Node's `spec.providerID` (see below) |
| `--require-provider-id` | `false` | Block node: fail `NodeGetInfo` (Unavailable, so the registrar retries) until the Node has a `vergeos://` providerID, instead of falling back to the Node name. Helm: `requireProviderId`. Set `true` on Cluster API / k0s clusters running the CCM |
| `--pool-vm-id` | integer | Pool VM for block drives (block controller only) |

API credentials are read from environment variables (`VERGEOS_HOST`, `VERGEOS_API_KEY`, `VERGEOS_VERIFY_SSL`), typically sourced from a Kubernetes Secret.

## Dependencies

| Package | Purpose |
|---------|---------|
| [govergeos](https://github.com/verge-io/govergeos) | VergeOS Go SDK — all API interactions |
| [container-storage-interface/spec](https://github.com/container-storage-interface/spec) | CSI protobuf/gRPC definitions |
| [k8s.io/mount-utils](https://pkg.go.dev/k8s.io/mount-utils) | Mount/unmount, SafeFormatAndMount |
| [k8s.io/klog/v2](https://pkg.go.dev/k8s.io/klog/v2) | Kubernetes-style structured logging |

## License

Apache License 2.0 — see [LICENSE](LICENSE) for details.
