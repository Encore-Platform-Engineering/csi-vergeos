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
2. **ControllerPublishVolume** — moves the drive from the pool VM to the target node's VM. The VM is found from the Node's `spec.providerID`, not its name: the node plugin reads its own Node (needs `get nodes`) and reports `uuid:<uuid>` for `vergeos://<uuid>` (Cluster API) or `key:<n>` for legacy `vergeos://<n>`; with no providerID yet it reports `name:<node name>` (with a warning) unless `--require-provider-id` is set. Detach finds the VM from the drive's current owner, not the node ID. Lookups skip snapshot VMs and fail if more than one VM matches. Bare node names from older installs still resolve by VM name.
3. **NodeStageVolume** — discovers the device by serial in `/dev/disk/by-id/`, formats (EXT4), and mounts to a staging path.
4. **NodePublishVolume** — bind-mounts the staging path into the pod.
5. **ControllerUnpublishVolume** — moves the drive back to the pool VM when the pod is done.

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

To install only the NAS driver (skip block):

```bash
helm install vergeos-csi verge-io/vergeos-csi \
  --namespace kube-system \
  --set vergeos.host=https://<VERGEOS_HOST> \
  --set vergeos.apiKey=<API_KEY> \
  --set block.enabled=false
```

On **k0s**, kubelet uses `/var/lib/k0s/kubelet`; add `--set kubeletDir=/var/lib/k0s/kubelet`. The value is used for the plugin socket, registration dir, the `--kubelet-registration-path`, and the Bidirectional pods mount. A mismatch means the node plugin never registers.

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
| `nas.enabled` | `true` | Deploy the NAS driver |
| `block.enabled` | `true` | Deploy the Block driver |
| `block.poolVmId` | | **Required** when block is enabled. VergeOS VM ID to hold idle block drives |
| `nas.storageClass.nasServiceName` | `k8s-nas` | VergeOS NAS service name |
| `kubeletDir` | `/var/lib/kubelet` | Kubelet root dir on nodes. Set to `/var/lib/k0s/kubelet` on k0s |
| `logLevel` | `5` | klog verbosity (0–10) |

### Raw manifests

The manifests in `deploy/kubernetes/` are **default-kubelet-only** (`/var/lib/kubelet`). They are not templated. For k0s or any other kubelet dir, use the Helm chart (`kubeletDir`) or generate manifests with `helm template --set kubeletDir=<dir> charts/vergeos-csi`.

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
make docker-push      # Build and push to ghcr.io/verge-io/csi-vergeos
make test             # All tests with -race

CI (`.github/workflows/ci.yml`) runs `go vet`, `go test` and a `docker build` on every PR. Pushing a `v*` tag also pushes `ghcr.io/encore-platform-engineering/csi-vergeos:<tag>`.
go vet ./...          # Static analysis
```

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
