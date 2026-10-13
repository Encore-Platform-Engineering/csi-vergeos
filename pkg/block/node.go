package block

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	"github.com/verge-io/csi-vergeos/pkg/util"
)

// supportedFsTypes is the allowlist of filesystems the node image can format.
// ext4 comes from e2fsprogs, xfs from xfsprogs (see Dockerfile).
var supportedFsTypes = map[string]bool{"ext4": true, "xfs": true}

// BlockNode implements driver.NodeBackend for block volumes.
type BlockNode struct {
	mounter   BlockMounter
	discovery DeviceDiscovery
}

// NewBlockNode creates a block node backend.
func NewBlockNode() *BlockNode {
	return &BlockNode{
		mounter:   util.NewSafeMounter(),
		discovery: util.NewDeviceFinder(),
	}
}

// mountSettings extracts the filesystem type (default ext4) and mount flags
// from a filesystem-mode capability, rejecting unsupported filesystems.
func mountSettings(volCap *csi.VolumeCapability) (fsType string, flags []string, err error) {
	fsType = "ext4"
	if m := volCap.GetMount(); m != nil {
		if m.FsType != "" {
			fsType = m.FsType
		}
		flags = m.MountFlags
	}
	if !supportedFsTypes[fsType] {
		return "", nil, status.Errorf(codes.InvalidArgument, "unsupported fsType %q: supported types are ext4 and xfs", fsType)
	}
	return fsType, flags, nil
}

func (n *BlockNode) NodeStageVolume(_ context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if req.StagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}

	stagingPath := req.StagingTargetPath

	// Raw block volumes are handed to the pod as a device at publish time;
	// nothing is formatted or mounted at staging.
	if req.VolumeCapability.GetBlock() != nil {
		klog.Infof("Staging raw block volume %s: nothing to format or mount", req.VolumeId)
		return &csi.NodeStageVolumeResponse{}, nil
	}

	fsType, mountFlags, err := mountSettings(req.VolumeCapability)
	if err != nil {
		return nil, err
	}

	// Idempotency: check if already staged.
	mounted, err := n.mounter.IsMounted(stagingPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "checking mount status: %v", err)
	}
	if mounted {
		klog.Infof("Volume %s already staged at %s (idempotent)", req.VolumeId, stagingPath)
		return &csi.NodeStageVolumeResponse{}, nil
	}

	// Get the drive serial from publish context (set by ControllerPublishVolume).
	serial := req.PublishContext["serial"]
	if serial == "" {
		return nil, status.Error(codes.InvalidArgument, "publish context missing 'serial'")
	}

	// Discover the block device by serial.
	devicePath, err := n.discovery.FindBySerial(serial)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "block device with serial %q not found: %v", serial, err)
	}

	// Create staging directory.
	if err := util.EnsureDirectory(stagingPath); err != nil {
		return nil, status.Errorf(codes.Internal, "creating staging directory: %v", err)
	}

	// Format (if needed) and mount the device to the staging path.
	klog.Infof("Staging volume %s: device=%s, fs=%s, target=%s", req.VolumeId, devicePath, fsType, stagingPath)
	if err := n.mounter.FormatAndMount(devicePath, stagingPath, fsType, mountFlags); err != nil {
		return nil, status.Errorf(codes.Internal, "format and mount failed: %v", err)
	}

	return &csi.NodeStageVolumeResponse{}, nil
}

func (n *BlockNode) NodeUnstageVolume(_ context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if req.StagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}

	stagingPath := req.StagingTargetPath

	// Idempotency: check if not staged.
	mounted, err := n.mounter.IsMounted(stagingPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "checking mount status: %v", err)
	}
	if !mounted {
		klog.Infof("Volume %s not staged at %s (idempotent)", req.VolumeId, stagingPath)
		return &csi.NodeUnstageVolumeResponse{}, nil
	}

	klog.Infof("Unstaging volume %s from %s", req.VolumeId, stagingPath)
	if err := n.mounter.Unmount(stagingPath); err != nil {
		return nil, status.Errorf(codes.Internal, "unmount staging failed: %v", err)
	}

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (n *BlockNode) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if req.TargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}
	if req.StagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}

	targetPath := req.TargetPath

	// Idempotency: check if already mounted.
	mounted, err := n.mounter.IsMounted(targetPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "checking mount status: %v", err)
	}
	if mounted {
		klog.Infof("Volume %s already published at %s (idempotent)", req.VolumeId, targetPath)
		return &csi.NodePublishVolumeResponse{}, nil
	}

	// Bind-mount from staging to target (filesystem mode) or the device node
	// itself onto a target file (raw block mode).
	options := []string{"bind"}
	if req.Readonly {
		options = append(options, "ro")
	}

	source := req.StagingTargetPath
	if req.VolumeCapability.GetBlock() != nil {
		serial := req.PublishContext["serial"]
		if serial == "" {
			return nil, status.Error(codes.InvalidArgument, "publish context missing 'serial'")
		}
		devicePath, err := n.discovery.FindBySerial(serial)
		if err != nil {
			return nil, status.Errorf(codes.NotFound, "block device with serial %q not found: %v", serial, err)
		}
		source = devicePath
		if err := ensureFile(targetPath); err != nil {
			return nil, status.Errorf(codes.Internal, "creating target file: %v", err)
		}
	} else {
		if err := util.EnsureDirectory(targetPath); err != nil {
			return nil, status.Errorf(codes.Internal, "creating target directory: %v", err)
		}
	}

	klog.Infof("Publishing volume %s: bind mount %s -> %s", req.VolumeId, source, targetPath)
	if err := n.mounter.Mount(source, targetPath, "", options); err != nil {
		return nil, status.Errorf(codes.Internal, "bind mount failed: %v", err)
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

// ensureFile creates an empty regular file (and parent directories) to serve
// as the bind-mount target for a raw block device.
func ensureFile(path string) error {
	if err := util.EnsureDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o640)
	if err != nil {
		return fmt.Errorf("creating %q: %w", path, err)
	}
	return f.Close()
}

func (n *BlockNode) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if req.TargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}

	targetPath := req.TargetPath

	// Idempotency: check if not mounted.
	mounted, err := n.mounter.IsMounted(targetPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "checking mount status: %v", err)
	}
	if !mounted {
		klog.Infof("Volume %s not published at %s (idempotent)", req.VolumeId, targetPath)
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	klog.Infof("Unpublishing volume %s from %s", req.VolumeId, targetPath)
	if err := n.mounter.Unmount(targetPath); err != nil {
		return nil, status.Errorf(codes.Internal, "unmount failed: %v", err)
	}

	if err := util.CleanupMountPoint(targetPath); err != nil {
		klog.Warningf("Failed to clean up mount point %s: %v", targetPath, err)
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (n *BlockNode) NodeGetCapabilities() []*csi.NodeServiceCapability {
	return []*csi.NodeServiceCapability{
		{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
				},
			},
		},
	}
}
