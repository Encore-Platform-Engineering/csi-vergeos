package block

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	vergeos "github.com/verge-io/govergeos"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	"github.com/verge-io/csi-vergeos/pkg/driver"
)

// BlockController implements driver.ControllerBackend for block volumes.
type BlockController struct {
	vmDrives      VMDriveClient
	vms           VMClient
	poolVMID      int // VM $key where drives are created before attachment
	poolMachineID int // VM internal machine ID for drive assignment
}

// NewBlockController creates a block controller backend using a govergeos client.
// poolVMID is the VergeOS VM $key that acts as a holding area for unattached drives.
// It resolves the VM's internal machine ID needed for drive assignment.
func NewBlockController(ctx context.Context, client *vergeos.Client, poolVMID int) (*BlockController, error) {
	poolVM, err := client.VMs.Get(ctx, poolVMID)
	if err != nil {
		return nil, fmt.Errorf("resolving pool VM %d: %w", poolVMID, err)
	}
	klog.Infof("Pool VM %d (%s) has machine ID %d", poolVMID, poolVM.Name, poolVM.Machine)
	return &BlockController{
		vmDrives:      client.VMDrives,
		vms:           client.VMs,
		poolVMID:      poolVMID,
		poolMachineID: poolVM.Machine,
	}, nil
}

func (c *BlockController) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}

	driveName := driver.VolumeNamePrefix + req.Name
	serial := deriveSerial(req.Name)

	// Idempotency: check if drive already exists on the pool VM.
	// List uses machine ID (internal reference) for filtering.
	existingDrives, err := c.vmDrives.List(ctx, c.poolMachineID)
	if err != nil && !vergeos.IsNotFoundError(err) {
		return nil, status.Errorf(codes.Internal, "listing drives on pool VM: %v", err)
	}

	for _, d := range existingDrives {
		if d.Name == driveName {
			klog.Infof("Block drive %q already exists on pool VM (id=%d), reusing (idempotent)", driveName, d.ID.Int())
			return c.buildCreateResponse(d), nil
		}
	}

	// If not found on pool VM, check if drive was already published to a node VM.
	// This handles the case where CreateVolume is retried after the drive was
	// already created and moved to a node via ControllerPublishVolume.
	allVMs, err := c.vms.List(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing VMs for idempotency check: %v", err)
	}
	for _, vm := range allVMs {
		if vm.Machine == c.poolMachineID {
			continue // already checked pool VM above
		}
		drives, err := c.vmDrives.List(ctx, vm.Machine)
		if err != nil {
			continue // best effort
		}
		for _, d := range drives {
			if d.Name == driveName {
				klog.Infof("Block drive %q found on VM %d (id=%d), reusing (idempotent)", driveName, vm.ID.Int(), d.ID.Int())
				return c.buildCreateResponse(d), nil
			}
		}
	}

	// Calculate size.
	sizeBytes := int64(1073741824) // 1 GiB default
	if req.CapacityRange != nil && req.CapacityRange.RequiredBytes > 0 {
		sizeBytes = req.CapacityRange.RequiredBytes
	}

	interfaceType := req.Parameters["interface"]
	if interfaceType == "" {
		interfaceType = "virtio-scsi"
	}

	drive, err := c.vmDrives.Create(ctx, c.poolMachineID, &vergeos.VMDriveCreateRequest{
		Name:      driveName,
		Interface: interfaceType,
		Media:     "disk",
		SizeBytes: sizeBytes,
		Serial:    serial,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "creating VM drive: %v", err)
	}

	klog.Infof("Created block drive %q (id=%d, serial=%s) on pool VM %d", driveName, drive.ID.Int(), serial, c.poolVMID)
	return c.buildCreateResponse(*drive), nil
}

func (c *BlockController) buildCreateResponse(drive vergeos.VMDrive) *csi.CreateVolumeResponse {
	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      encodeVolumeID(drive.ID.Int()),
			CapacityBytes: drive.SizeBytes,
			VolumeContext: map[string]string{
				"serial":    drive.Serial,
				"driveName": drive.Name,
			},
		},
	}
}

func (c *BlockController) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	driveID, err := decodeVolumeID(req.VolumeId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volume ID: %v", err)
	}

	if err := c.vmDrives.Delete(ctx, driveID); err != nil {
		if vergeos.IsNotFoundError(err) {
			klog.Infof("Block drive %d already deleted (idempotent)", driveID)
			return &csi.DeleteVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal, "deleting VM drive %d: %v", driveID, err)
	}

	klog.Infof("Deleted block drive %d", driveID)
	return &csi.DeleteVolumeResponse{}, nil
}

// nodeVMInfo holds both the VM $key and internal machine ID for a node.
type nodeVMInfo struct {
	vmID      int // VM $key — used for hotplug actions
	machineID int // Internal machine reference — used for drive assignment
}

// resolveNodeVM looks up the VergeOS VM for a CSI node ID. The ID is
// `uuid:<vm uuid>`, `key:<vm $key>`, `name:<vm name>` or a bare VM name
// (legacy). Snapshot VMs are excluded and more than one match is an error.
func (c *BlockController) resolveNodeVM(ctx context.Context, nodeID string) (*nodeVMInfo, error) {
	kind, value, err := driver.ParseNodeID(nodeID)
	if err != nil {
		return nil, err
	}
	var filter string
	var match func(vm vergeos.VM) bool
	switch kind {
	case driver.NodeIDKindUUID:
		filter = fmt.Sprintf("uuid eq '%s' and is_snapshot eq false", value)
		match = func(vm vergeos.VM) bool { return strings.EqualFold(vm.UUID, value) }
	case driver.NodeIDKindKey:
		filter = fmt.Sprintf("$key eq %s and is_snapshot eq false", value)
		match = func(vm vergeos.VM) bool { return strconv.Itoa(vm.ID.Int()) == value }
	default:
		filter = fmt.Sprintf("name eq '%s' and is_snapshot eq false", value)
		match = func(vm vergeos.VM) bool { return vm.Name == value }
	}

	vms, err := c.vms.List(ctx, vergeos.WithFilter(filter))
	if err != nil {
		return nil, fmt.Errorf("listing VMs to find node %q: %w", nodeID, err)
	}
	var found []vergeos.VM
	for _, vm := range vms {
		if !vm.IsSnapshot && match(vm) {
			found = append(found, vm)
		}
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("no VergeOS VM found matching node %q", nodeID)
	case 1:
		return &nodeVMInfo{vmID: found[0].ID.Int(), machineID: found[0].Machine}, nil
	default:
		return nil, fmt.Errorf("%d VergeOS VMs match node %q; refusing to guess", len(found), nodeID)
	}
}

func (c *BlockController) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if req.NodeId == "" {
		return nil, status.Error(codes.InvalidArgument, "node ID is required")
	}

	driveID, err := decodeVolumeID(req.VolumeId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volume ID: %v", err)
	}

	// Resolve the Kubernetes node name to a VergeOS VM.
	target, err := c.resolveNodeVM(ctx, req.NodeId)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "resolving node VM: %v", err)
	}

	// Get the drive to check current state.
	drive, err := c.vmDrives.Get(ctx, driveID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "drive %d not found: %v", driveID, err)
	}

	// Idempotency: if already attached to the target, return success.
	if drive.Machine == target.machineID {
		klog.Infof("Drive %d already attached to VM %d (idempotent)", driveID, target.vmID)
		return &csi.ControllerPublishVolumeResponse{
			PublishContext: map[string]string{
				"serial": drive.Serial,
			},
		}, nil
	}

	// Move the drive to the target VM using the internal machine ID.
	// Set OrderID to the drive ID to avoid uniqueness conflicts when multiple
	// CSI drives land on the same VM (VergeOS requires unique order per VM).
	_, err = c.vmDrives.Update(ctx, driveID, &vergeos.VMDriveUpdateRequest{
		Machine: &target.machineID,
		OrderID: &driveID,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "moving drive %d to VM %d (machine %d): %v", driveID, target.vmID, target.machineID, err)
	}

	// Hotplug the drive so the guest OS can see it.
	if err := c.vmDrives.HotplugDrive(ctx, target.vmID, driveID); err != nil {
		return nil, status.Errorf(codes.Internal, "hotplugging drive %d into VM %d: %v", driveID, target.vmID, err)
	}

	klog.Infof("Attached and hotplugged drive %d to VM %d (machine=%d, serial=%s)", driveID, target.vmID, target.machineID, drive.Serial)
	return &csi.ControllerPublishVolumeResponse{
		PublishContext: map[string]string{
			"serial": drive.Serial,
		},
	}, nil
}

func (c *BlockController) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	driveID, err := decodeVolumeID(req.VolumeId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volume ID: %v", err)
	}

	// Get drive to check current state.
	drive, err := c.vmDrives.Get(ctx, driveID)
	if err != nil {
		if vergeos.IsNotFoundError(err) {
			klog.Infof("Drive %d not found during unpublish (idempotent)", driveID)
			return &csi.ControllerUnpublishVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal, "getting drive %d: %v", driveID, err)
	}

	// Idempotency: if already on the pool VM (detached), return success.
	if drive.Machine == c.poolMachineID {
		klog.Infof("Drive %d already on pool VM %d (idempotent)", driveID, c.poolVMID)
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}

	// Hot-unplug the drive from the current VM before moving it.
	// Resolve which VM currently owns it so we can send the hotplug action.
	if drive.PowerState == "online" {
		if req.NodeId != "" {
			nodeVM, err := c.resolveNodeVM(ctx, req.NodeId)
			if err != nil {
				klog.Warningf("Resolving node %q for hot-unplug of drive %d failed: %v (will still move)", req.NodeId, driveID, err)
			} else if err := c.vmDrives.HotUnplugDrive(ctx, nodeVM.vmID, driveID); err != nil {
				klog.Warningf("Hot-unplug drive %d from VM %d failed: %v (will still move)", driveID, nodeVM.vmID, err)
			}
		}
	}

	// Move drive back to pool VM.
	_, err = c.vmDrives.Update(ctx, driveID, &vergeos.VMDriveUpdateRequest{
		Machine: &c.poolMachineID,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "detaching drive %d: %v", driveID, err)
	}

	klog.Infof("Detached drive %d back to pool VM %d (machine=%d)", driveID, c.poolVMID, c.poolMachineID)
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (c *BlockController) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	// Block supports ReadWriteOnce only.
	for _, cap := range req.VolumeCapabilities {
		if cap.AccessMode.Mode != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER {
			return &csi.ValidateVolumeCapabilitiesResponse{
				Message: fmt.Sprintf("block driver only supports SINGLE_NODE_WRITER, got %s", cap.AccessMode.Mode),
			}, nil
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.VolumeCapabilities,
		},
	}, nil
}

func (c *BlockController) ControllerGetCapabilities() []*csi.ControllerServiceCapability {
	return []*csi.ControllerServiceCapability{
		{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
				},
			},
		},
		{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
				},
			},
		},
	}
}
