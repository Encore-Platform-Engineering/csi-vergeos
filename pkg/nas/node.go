package nas

import (
	"context"
	"fmt"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	"github.com/verge-io/csi-vergeos/pkg/util"
)

// NASNode implements driver.NodeBackend for NAS volumes.
type NASNode struct {
	mounter NFSMounter
}

// NewNASNode creates a NAS node backend.
func NewNASNode() *NASNode {
	return &NASNode{
		mounter: util.NewMounter(),
	}
}

func (n *NASNode) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if req.TargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}

	targetPath := req.TargetPath

	// NFS exports are filesystems; raw block volumes cannot be served.
	if req.VolumeCapability.GetBlock() != nil {
		return nil, status.Error(codes.InvalidArgument, "NAS volumes do not support volumeMode: Block")
	}

	// Idempotency: check if already mounted.
	mounted, err := n.mounter.IsMounted(targetPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "checking mount status: %v", err)
	}
	if mounted {
		klog.Infof("Volume %s already mounted at %s (idempotent)", req.VolumeId, targetPath)
		return &csi.NodePublishVolumeResponse{}, nil
	}

	// Get NAS service IP and volume name from volume context.
	nasIP := req.VolumeContext["nasServiceIP"]
	if nasIP == "" {
		return nil, status.Error(codes.InvalidArgument, "volume context missing 'nasServiceIP'")
	}
	volumeName := req.VolumeContext["volumeName"]
	if volumeName == "" {
		return nil, status.Error(codes.InvalidArgument, "volume context missing 'volumeName'")
	}

	// Build NFS mount source: <nas-ip>:/mnt/<volume-name>
	// VergeOS NAS exports volumes under /mnt/.
	source := fmt.Sprintf("%s:/mnt/%s", nasIP, volumeName)

	options := nfsMountOptions(req.VolumeCapability.GetMount().GetMountFlags(), req.Readonly)

	klog.Infof("NFS mounting %s to %s", source, targetPath)
	if err := n.mounter.MountNFS(source, targetPath, options); err != nil {
		return nil, status.Errorf(codes.Internal, "NFS mount failed: %v", err)
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (n *NASNode) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if req.TargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}

	targetPath := req.TargetPath

	klog.Infof("Unmounting NFS volume %s from %s", req.VolumeId, targetPath)
	if err := n.mounter.Unmount(targetPath); err != nil {
		return nil, status.Errorf(codes.Internal, "unmount failed: %v", err)
	}

	// Clean up mount point directory.
	if err := util.CleanupMountPoint(targetPath); err != nil {
		klog.Warningf("Failed to clean up mount point %s: %v", targetPath, err)
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeStageVolume is not used by NAS (no STAGE_UNSTAGE_VOLUME capability).
func (n *NASNode) NodeStageVolume(_ context.Context, _ *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NAS driver does not use staging")
}

// NodeUnstageVolume is not used by NAS.
func (n *NASNode) NodeUnstageVolume(_ context.Context, _ *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NAS driver does not use staging")
}

func (n *NASNode) NodeGetCapabilities() []*csi.NodeServiceCapability {
	// NAS does NOT advertise STAGE_UNSTAGE_VOLUME — NFS doesn't need staging.
	return []*csi.NodeServiceCapability{}
}

// nfsMountOptions merges StorageClass mountOptions with the driver defaults.
//
// Defaults: "hard", and "nolock" because rpc.statd is not available in the CSI
// node container. User options win: "hard" is dropped when the user picks
// soft/hard, and "nolock" is dropped when the user sets lock/nolock/local_lock
// or selects NFSv4 (nfsvers/vers=4*, v4/v4.1/v4.2, which has in-protocol locking and needs no
// statd).
func nfsMountOptions(userFlags []string, readonly bool) []string {
	var (
		hasHardSoft, hasLock, nfsv4, hasRO bool
		options                            []string
	)
	for _, f := range userFlags {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		switch k, v, _ := strings.Cut(f, "="); k {
		case "hard", "soft":
			hasHardSoft = true
		case "lock", "nolock", "local_lock":
			hasLock = true
		case "nfsvers", "vers":
			if strings.HasPrefix(v, "4") {
				nfsv4 = true
			}
		case "v4", "v4.0", "v4.1", "v4.2":
			nfsv4 = true
		case "ro":
			hasRO = true
		}
		options = append(options, f)
	}
	if !hasHardSoft {
		options = append([]string{"hard"}, options...)
	}
	if !hasLock && !nfsv4 {
		options = append(options, "nolock")
	}
	if readonly && !hasRO {
		options = append(options, "ro")
	}
	return options
}
