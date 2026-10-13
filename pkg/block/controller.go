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

	locks keyedLock // per-drive / per-volume-name serialization
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

// defaultVolumeBytes is the size used when the request gives no capacity.
const defaultVolumeBytes int64 = 1 << 30 // 1 GiB

// volumeSize resolves the requested capacity range to a size in bytes and the
// limit (0 = unbounded).
func volumeSize(cr *csi.CapacityRange) (size, limit int64, err error) {
	size = defaultVolumeBytes
	if cr == nil {
		return size, 0, nil
	}
	if cr.RequiredBytes < 0 || cr.LimitBytes < 0 {
		return 0, 0, status.Error(codes.InvalidArgument, "capacity range must not be negative")
	}
	limit = cr.LimitBytes
	if cr.RequiredBytes > 0 {
		size = cr.RequiredBytes
	}
	if limit > 0 {
		if cr.RequiredBytes > limit {
			return 0, 0, status.Errorf(codes.OutOfRange, "required bytes %d exceed limit bytes %d", cr.RequiredBytes, limit)
		}
		if size > limit {
			size = limit // default size does not fit under the limit
		}
	}
	return size, limit, nil
}

func (c *BlockController) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}
	if err := checkCapabilities(req.VolumeCapabilities...); err != nil {
		return nil, err
	}
	sizeBytes, limitBytes, err := volumeSize(req.CapacityRange)
	if err != nil {
		return nil, err
	}
	// Only an explicit required_bytes is a minimum; the default size is not.
	var minBytes int64
	if req.CapacityRange != nil {
		minBytes = req.CapacityRange.RequiredBytes
	}

	driveName := driver.VolumeNamePrefix + req.Name
	serial := deriveSerial(req.Name)

	// Concurrent retries for the same name must not both create a drive.
	unlock, err := c.locks.lock(ctx, "create:"+driveName)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Idempotency: look only at the pool VM, with one filtered list call.
	// Trade-off: a drive that already left the pool VM (published to a node,
	// or parked on a previous pool VM) is not found, so a same-name retry
	// would create a duplicate. That cannot happen in the Kubernetes flow: a
	// drive is only published after CreateVolume succeeded and the PV exists,
	// and the provisioner stops calling CreateVolume once it does. In return
	// there is no O(VMs) scan and drives of other clusters sharing this
	// VergeOS are never considered.
	// Any list failure must abort: treating it as "not found" would create a
	// duplicate drive.
	existingDrives, err := c.vmDrives.List(ctx, c.poolMachineID)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "listing drives on pool VM: %v", err)
	}

	var existing []vergeos.VMDrive
	for _, d := range existingDrives {
		if d.Name == driveName {
			existing = append(existing, d)
		}
	}
	switch len(existing) {
	case 0:
	case 1:
		d := existing[0]
		if (minBytes > 0 && d.SizeBytes < minBytes) || (limitBytes > 0 && d.SizeBytes > limitBytes) {
			return nil, status.Errorf(codes.AlreadyExists, "volume %q already exists with size %d bytes, incompatible with requested %d (limit %d)", req.Name, d.SizeBytes, sizeBytes, limitBytes)
		}
		klog.Infof("Block drive %q already exists on pool VM (id=%d), reusing (idempotent)", driveName, d.ID.Int())
		return c.buildCreateResponse(d), nil
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "%d drives named %q exist on pool VM; refusing to guess", len(existing), driveName)
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
		return nil, status.Errorf(codes.Unavailable, "creating VM drive: %v", err)
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

	unlock, err := c.locks.lock(ctx, fmt.Sprintf("drive:%d", driveID))
	if err != nil {
		return nil, err
	}
	defer unlock()

	drive, err := c.vmDrives.Get(ctx, driveID)
	if err != nil {
		if vergeos.IsNotFoundError(err) {
			klog.Infof("Block drive %d already deleted (idempotent)", driveID)
			return &csi.DeleteVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Unavailable, "getting drive %d: %v", driveID, err)
	}
	// Never delete a drive this driver did not create (static PV, stale ID, VM boot disk).
	if !strings.HasPrefix(drive.Name, driver.VolumeNamePrefix) {
		klog.Warningf("Drive %d (%q) is not a %q volume; refusing to delete it", driveID, drive.Name, driver.VolumeNamePrefix)
		return &csi.DeleteVolumeResponse{}, nil
	}
	if drive.Machine != c.poolMachineID {
		return nil, status.Errorf(codes.FailedPrecondition, "drive %d is still attached (machine %d); unpublish it before deleting", driveID, drive.Machine)
	}

	if err := c.vmDrives.Delete(ctx, driveID); err != nil {
		if vergeos.IsNotFoundError(err) {
			klog.Infof("Block drive %d already deleted (idempotent)", driveID)
			return &csi.DeleteVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Unavailable, "deleting VM drive %d: %v", driveID, err)
	}

	klog.Infof("Deleted block drive %d", driveID)
	return &csi.DeleteVolumeResponse{}, nil
}

// nodeVMInfo holds both the VM $key and internal machine ID for a node.
type nodeVMInfo struct {
	vmID      int // VM $key — used for hotplug actions
	machineID int // Internal machine reference — used for drive assignment
}

// escapeFilterString escapes a value for use inside a single-quoted VergeOS
// filter string.
func escapeFilterString(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s)
}

// findVM lists non-snapshot VMs with `filter`, keeps those accepted by match
// (the API filter is re-checked client-side) and requires exactly one.
// Returns gRPC errors: Unavailable (API failure), NotFound, FailedPrecondition
// (ambiguous).
func (c *BlockController) findVM(ctx context.Context, what, filter string, match func(vergeos.VM) bool) (*nodeVMInfo, error) {
	vms, err := c.vms.List(ctx, vergeos.WithFilter(filter+" and is_snapshot eq false"))
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "listing VMs to find %s: %v", what, err)
	}
	var found []vergeos.VM
	for _, vm := range vms {
		if !vm.IsSnapshot && match(vm) {
			found = append(found, vm)
		}
	}
	switch len(found) {
	case 0:
		return nil, status.Errorf(codes.NotFound, "no VergeOS VM found matching %s", what)
	case 1:
		return &nodeVMInfo{vmID: found[0].ID.Int(), machineID: found[0].Machine}, nil
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "%d VergeOS VMs match %s; refusing to guess", len(found), what)
	}
}

// resolveNodeVM looks up the VergeOS VM for a CSI node ID. The ID is
// `uuid:<vm uuid>`, `key:<vm $key>`, `name:<vm name>` or a bare VM name
// (legacy). Malformed IDs are InvalidArgument.
func (c *BlockController) resolveNodeVM(ctx context.Context, nodeID string) (*nodeVMInfo, error) {
	kind, value, err := driver.ParseNodeID(nodeID)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	what := fmt.Sprintf("node %q", nodeID)
	switch kind {
	case driver.NodeIDKindUUID:
		return c.findVM(ctx, what, fmt.Sprintf("uuid eq '%s'", value),
			func(vm vergeos.VM) bool { return strings.EqualFold(vm.UUID, value) })
	case driver.NodeIDKindKey:
		return c.findVM(ctx, what, fmt.Sprintf("$key eq %s", value),
			func(vm vergeos.VM) bool { return strconv.Itoa(vm.ID.Int()) == value })
	default:
		return c.findVM(ctx, what, fmt.Sprintf("name eq '%s'", escapeFilterString(value)),
			func(vm vergeos.VM) bool { return vm.Name == value })
	}
}

// vmForMachine finds the VM that owns internal machine ID machineID, i.e. the
// VM a drive is currently attached to.
func (c *BlockController) vmForMachine(ctx context.Context, machineID int) (*nodeVMInfo, error) {
	return c.findVM(ctx, fmt.Sprintf("machine %d", machineID), fmt.Sprintf("machine eq %d", machineID),
		func(vm vergeos.VM) bool { return vm.Machine == machineID })
}

func (c *BlockController) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if req.NodeId == "" {
		return nil, status.Error(codes.InvalidArgument, "node ID is required")
	}
	if req.VolumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	}
	if err := checkCapabilities(req.VolumeCapability); err != nil {
		return nil, err
	}

	driveID, err := decodeVolumeID(req.VolumeId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volume ID: %v", err)
	}

	// Serialize publish/unpublish of one drive so retries and concurrent
	// calls cannot interleave between the move and the hotplug.
	unlock, err := c.locks.lock(ctx, fmt.Sprintf("drive:%d", driveID))
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Resolve the CSI node ID (uuid:/key:/name:) to a VergeOS VM.
	target, err := c.resolveNodeVM(ctx, req.NodeId)
	if err != nil {
		return nil, err
	}

	drive, err := c.vmDrives.Get(ctx, driveID)
	if err != nil {
		if vergeos.IsNotFoundError(err) {
			return nil, status.Errorf(codes.NotFound, "drive %d not found: %v", driveID, err)
		}
		return nil, status.Errorf(codes.Unavailable, "getting drive %d: %v", driveID, err)
	}
	// Only drives created by this driver may be (un)published: never a VM
	// boot disk or an unrelated drive reached through a stale/static ID.
	if !strings.HasPrefix(drive.Name, driver.VolumeNamePrefix) {
		return nil, status.Errorf(codes.NotFound, "drive %d (%q) is not a %q volume", driveID, drive.Name, driver.VolumeNamePrefix)
	}

	publishContext := map[string]string{"serial": drive.Serial}

	switch drive.Machine {
	case target.machineID:
		// Already assigned to the target. A previous attempt may have moved
		// the drive and then failed to hotplug it, so only report success
		// once the drive is actually online.
		if drive.PowerState == "online" {
			klog.Infof("Drive %d already attached and online on VM %d (idempotent)", driveID, target.vmID)
			return &csi.ControllerPublishVolumeResponse{PublishContext: publishContext}, nil
		}
		klog.Infof("Drive %d assigned to VM %d but not online (state %q); hotplugging", driveID, target.vmID, drive.PowerState)
	case c.poolMachineID:
		// Move the drive to the target VM using the internal machine ID.
		// Set OrderID to the drive ID to avoid uniqueness conflicts when
		// multiple CSI drives land on the same VM (VergeOS requires unique
		// order per VM).
		_, err = c.vmDrives.Update(ctx, driveID, &vergeos.VMDriveUpdateRequest{
			Machine: &target.machineID,
			OrderID: &driveID,
		})
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "moving drive %d to VM %d (machine %d): %v", driveID, target.vmID, target.machineID, err)
		}
	default:
		// Attached to some other VM: moving it would yank a possibly mounted
		// device. ReadWriteOnce means it must be unpublished there first.
		return nil, status.Errorf(codes.FailedPrecondition, "drive %d is published to another node (machine %d); unpublish it first", driveID, drive.Machine)
	}

	// Hotplug the drive so the guest OS can see it. On failure the drive
	// stays assigned to the target and a retry lands in the case above.
	if err := c.vmDrives.HotplugDrive(ctx, target.vmID, driveID); err != nil {
		return nil, status.Errorf(codes.Unavailable, "hotplugging drive %d into VM %d: %v", driveID, target.vmID, err)
	}

	klog.Infof("Attached and hotplugged drive %d to VM %d (machine=%d, serial=%s)", driveID, target.vmID, target.machineID, drive.Serial)
	return &csi.ControllerPublishVolumeResponse{PublishContext: publishContext}, nil
}

func (c *BlockController) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	driveID, err := decodeVolumeID(req.VolumeId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volume ID: %v", err)
	}

	unlock, err := c.locks.lock(ctx, fmt.Sprintf("drive:%d", driveID))
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Get drive to check current state.
	drive, err := c.vmDrives.Get(ctx, driveID)
	if err != nil {
		if vergeos.IsNotFoundError(err) {
			klog.Infof("Drive %d not found during unpublish (idempotent)", driveID)
			return &csi.ControllerUnpublishVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Unavailable, "getting drive %d: %v", driveID, err)
	}
	if !strings.HasPrefix(drive.Name, driver.VolumeNamePrefix) {
		klog.Warningf("Drive %d (%q) is not a %q volume; ignoring unpublish", driveID, drive.Name, driver.VolumeNamePrefix)
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}

	// Idempotency: if already on the pool VM (detached), return success.
	if drive.Machine == c.poolMachineID {
		klog.Infof("Drive %d already on pool VM %d (idempotent)", driveID, c.poolVMID)
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}

	// Work out which VM currently holds the drive. A node ID that resolves to
	// the holder is authoritative. Otherwise (no node ID, node VM gone, or a
	// late call for a node that no longer holds the drive) look the holder up
	// by the drive's machine so a drive stranded on a deleted VM still heals.
	var node *nodeVMInfo
	if req.NodeId != "" {
		n, err := c.resolveNodeVM(ctx, req.NodeId)
		switch {
		case err == nil:
			node = n
		case status.Code(err) == codes.NotFound:
			klog.Infof("Node %q has no VergeOS VM; locating the holder of drive %d by machine", req.NodeId, driveID)
		default:
			return nil, err
		}
	}

	var owner *nodeVMInfo
	if node != nil && node.machineID == drive.Machine {
		owner = node
	} else {
		holder, err := c.vmForMachine(ctx, drive.Machine)
		switch {
		case err == nil:
			if req.NodeId != "" {
				// Late or duplicate unpublish for a node that does not hold
				// the drive; a live VM holds it now, leave it alone.
				klog.Infof("Drive %d is held by VM %d, not node %q; nothing to do", driveID, holder.vmID, req.NodeId)
				return &csi.ControllerUnpublishVolumeResponse{}, nil
			}
			owner = holder
		case status.Code(err) == codes.NotFound:
			// The holding VM is gone: nothing to unplug from, just move the
			// drive back to the pool so it is not stranded.
		default:
			return nil, err
		}
	}

	// Hot-unplug before moving. Never move a drive the guest may still have:
	// on failure return a retryable error and leave the drive where it is.
	if owner != nil && drive.PowerState == "online" {
		if err := c.vmDrives.HotUnplugDrive(ctx, owner.vmID, driveID); err != nil && !vergeos.IsNotFoundError(err) {
			return nil, status.Errorf(codes.Unavailable, "hot-unplugging drive %d from VM %d: %v", driveID, owner.vmID, err)
		}
	}

	// Move drive back to pool VM.
	_, err = c.vmDrives.Update(ctx, driveID, &vergeos.VMDriveUpdateRequest{
		Machine: &c.poolMachineID,
	})
	if err != nil {
		if vergeos.IsNotFoundError(err) {
			return &csi.ControllerUnpublishVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Unavailable, "detaching drive %d: %v", driveID, err)
	}

	klog.Infof("Detached drive %d back to pool VM %d (machine=%d)", driveID, c.poolVMID, c.poolMachineID)
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (c *BlockController) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if len(req.VolumeCapabilities) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	// Block supports single-node volumes (filesystem or raw block mode).
	for _, cap := range req.VolumeCapabilities {
		if msg := unsupportedCapability(cap); msg != "" {
			return &csi.ValidateVolumeCapabilitiesResponse{Message: msg}, nil
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.VolumeCapabilities,
		},
	}, nil
}

// unsupportedCapability returns why cap cannot be served by the block driver,
// or "" if it is supported. A drive can be attached to one VM at a time, so
// only SINGLE_NODE_* access modes are allowed. Raw block mode (volumeMode:
// Block) is allowed, except SINGLE_NODE_READER_ONLY: the node plugin hands the
// device node to the pod by bind mount, and a read-only bind mount does not
// stop writes to a device node.
func unsupportedCapability(cap *csi.VolumeCapability) string {
	if cap == nil {
		return "volume capability is nil"
	}
	if cap.AccessMode == nil {
		return "access mode is required"
	}
	if cap.GetBlock() != nil && cap.AccessMode.Mode == csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY {
		return "block driver does not support read-only raw block volumes"
	}
	switch cap.AccessMode.Mode {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:
		return ""
	}
	return fmt.Sprintf("block driver only supports SINGLE_NODE_* access modes, got %s", cap.AccessMode.Mode)
}

// checkCapabilities returns InvalidArgument if caps is empty or any entry is
// not supported.
func checkCapabilities(caps ...*csi.VolumeCapability) error {
	if len(caps) == 0 {
		return status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	for _, cap := range caps {
		if msg := unsupportedCapability(cap); msg != "" {
			return status.Error(codes.InvalidArgument, msg)
		}
	}
	return nil
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
