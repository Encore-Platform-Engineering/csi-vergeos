package nas

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	vergeos "github.com/verge-io/govergeos"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	"github.com/verge-io/csi-vergeos/pkg/driver"
)

// NASController implements driver.ControllerBackend for NAS volumes.
type NASController struct {
	nasServices   NASServiceClient
	volumes       VolumeClient
	nfsShares     NFSShareClient
	vmNICs        VMNICClient
	vms           VMClient
	machineStatus MachineStatusClient

	// nasServiceMu serializes on-demand NAS service creation so concurrent
	// CreateVolume calls cannot create duplicate services on the same VM.
	nasServiceMu sync.Mutex
}

// nfsExportConfig is the validated NFS access policy derived from StorageClass
// parameters.
type nfsExportConfig struct {
	allowAll     bool
	allowedHosts string // comma-delimited, only meaningful when !allowAll
	squash       string
}

// parseExportConfig validates the access-control StorageClass parameters.
//
//   - allowedHosts: comma-separated IPs/CIDRs (typically node subnets) that may
//     mount the export. Required unless allowAll is "true".
//   - allowAll: "true" explicitly opts in to a world-accessible export.
//   - squash: root_squash (default), all_squash, or no_root_squash.
func parseExportConfig(params map[string]string) (*nfsExportConfig, error) {
	cfg := &nfsExportConfig{squash: "root_squash"}

	switch v := strings.TrimSpace(params["allowAll"]); v {
	case "", "false":
	case "true":
		cfg.allowAll = true
	default:
		return nil, status.Errorf(codes.InvalidArgument, "invalid allowAll %q: must be \"true\" or \"false\"", v)
	}

	if raw := params["allowedHosts"]; strings.TrimSpace(raw) != "" {
		var hosts []string
		for _, h := range strings.Split(raw, ",") {
			h = strings.TrimSpace(h)
			if h == "" {
				continue
			}
			if net.ParseIP(h) == nil {
				_, ipNet, err := net.ParseCIDR(h)
				if err != nil {
					return nil, status.Errorf(codes.InvalidArgument, "invalid allowedHosts entry %q: must be an IP address or CIDR", h)
				}
				if ones, _ := ipNet.Mask.Size(); ones == 0 {
					return nil, status.Errorf(codes.InvalidArgument, "allowedHosts entry %q matches every host: set 'allowAll: \"true\"' instead", h)
				}
			}
			hosts = append(hosts, h)
		}
		if cfg.allowAll && len(hosts) > 0 {
			return nil, status.Error(codes.InvalidArgument, "'allowAll: \"true\"' and 'allowedHosts' are mutually exclusive")
		}
		cfg.allowedHosts = strings.Join(hosts, ",")
	}

	if !cfg.allowAll && cfg.allowedHosts == "" {
		return nil, status.Error(codes.InvalidArgument,
			"StorageClass parameter 'allowedHosts' (comma-separated node IPs/CIDRs) is required; set 'allowAll: \"true\"' to explicitly export to every host")
	}

	if sq := strings.TrimSpace(params["squash"]); sq != "" {
		switch sq {
		case "root_squash", "all_squash", "no_root_squash":
			cfg.squash = sq
		default:
			return nil, status.Errorf(codes.InvalidArgument, "invalid squash %q: must be root_squash, all_squash or no_root_squash", sq)
		}
	}
	return cfg, nil
}

// NewNASController creates a NAS controller backend using a govergeos client.
func NewNASController(client *vergeos.Client) *NASController {
	return &NASController{
		nasServices:   client.NASServices,
		volumes:       client.Volumes,
		nfsShares:     client.VolumeNFSShares,
		vmNICs:        client.VMNICs,
		vms:           client.VMs,
		machineStatus: client.MachineStatus,
	}
}

func (c *NASController) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}

	nasServiceName := req.Parameters["nasServiceName"]
	if nasServiceName == "" {
		return nil, status.Error(codes.InvalidArgument, "StorageClass parameter 'nasServiceName' is required")
	}

	for _, volCap := range req.VolumeCapabilities {
		if volCap.GetBlock() != nil {
			return nil, status.Error(codes.InvalidArgument, "NAS volumes do not support volumeMode: Block")
		}
	}

	// Validate the export access policy before touching the VergeOS API.
	export, err := parseExportConfig(req.Parameters)
	if err != nil {
		return nil, err
	}

	// Parse optional nasServiceVM for on-demand service creation.
	var nasServiceVM int
	if vmStr := req.Parameters["nasServiceVM"]; vmStr != "" {
		v, parseErr := strconv.Atoi(vmStr)
		if parseErr != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid nasServiceVM %q: %v", vmStr, parseErr)
		}
		nasServiceVM = v
	}

	// Look up the NAS service, creating on demand if nasServiceVM is provided.
	nasSvc, err := c.resolveNASService(ctx, nasServiceName, nasServiceVM)
	if err != nil {
		return nil, err
	}
	nasServiceID := nasSvc.Key.Int()

	volumeName := driver.VolumeNamePrefix + req.Name

	// Idempotency: check if volume already exists.
	vol, err := c.volumes.GetByName(ctx, nasServiceID, volumeName)
	if err != nil && !vergeos.IsNotFoundError(err) {
		return nil, status.Errorf(codes.Internal, "checking for existing volume: %v", err)
	}

	if vol == nil {
		// Create the volume.
		sizeBytes := int64(1073741824) // 1 GiB default
		if req.CapacityRange != nil && req.CapacityRange.RequiredBytes > 0 {
			sizeBytes = req.CapacityRange.RequiredBytes
		}

		preferredTier := req.Parameters["preferredTier"]
		if preferredTier == "" {
			preferredTier = "1"
		}

		vol, err = c.volumes.Create(ctx, &vergeos.VolumeCreateRequest{
			Name:          volumeName,
			Service:       nasServiceID,
			MaxSize:       &sizeBytes,
			PreferredTier: &preferredTier,
		})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "creating volume: %v", err)
		}
		klog.Infof("Created NAS volume %q (id=%s) on service %d", volumeName, vol.ID, nasServiceID)
	} else {
		klog.Infof("NAS volume %q already exists (id=%s), reusing (idempotent)", volumeName, vol.ID)
	}

	// Idempotency: check if NFS share already exists.
	shareName := volumeName
	share, err := c.nfsShares.GetByName(ctx, vol.ID, shareName)
	if err != nil && !vergeos.IsNotFoundError(err) {
		return nil, status.Errorf(codes.Internal, "checking for existing NFS share: %v", err)
	}

	if share == nil {
		dataAccess := "rw"
		shareReq := &vergeos.VolumeNFSShareCreateRequest{
			Name:       shareName,
			Volume:     vol.ID,
			AllowAll:   &export.allowAll,
			DataAccess: &dataAccess,
			Squash:     &export.squash,
		}
		if !export.allowAll {
			shareReq.AllowedHosts = &export.allowedHosts
		}
		share, err = c.nfsShares.Create(ctx, shareReq)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "creating NFS share: %v", err)
		}
		klog.Infof("Created NFS share %q (id=%s) on volume %s", shareName, share.ID, vol.ID)
	} else {
		klog.Infof("NFS share %q already exists (id=%s), reusing (idempotent)", shareName, share.ID)
	}

	// Resolve NAS service IP for the node to mount NFS.
	nasServiceIP := req.Parameters["nasServiceIP"]
	if nasServiceIP == "" {
		ip, err := c.getNASServiceIP(ctx, nasSvc)
		if err != nil {
			klog.Warningf("Could not look up NAS service IP dynamically: %v", err)
			return nil, status.Errorf(codes.Internal, "NAS service IP required: set 'nasServiceIP' in StorageClass parameters or fix API lookup: %v", err)
		}
		nasServiceIP = ip
	}

	volumeID := encodeVolumeID(nasServiceID, vol.ID, share.ID)

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			CapacityBytes: vol.MaxSize,
			VolumeContext: map[string]string{
				"nasServiceID":   fmt.Sprintf("%d", nasServiceID),
				"nasServiceName": nasServiceName,
				"nasServiceIP":   nasServiceIP,
				"volumeID":       vol.ID,
				"nfsShareID":     share.ID,
				"volumeName":     volumeName,
			},
		},
	}, nil
}

func (c *NASController) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	_, volumeID, nfsShareID, err := decodeVolumeID(req.VolumeId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volume ID: %v", err)
	}

	// Delete the NFS share first.
	if err := c.nfsShares.Delete(ctx, nfsShareID); err != nil {
		if !vergeos.IsNotFoundError(err) {
			return nil, status.Errorf(codes.Internal, "deleting NFS share %s: %v", nfsShareID, err)
		}
		klog.Infof("NFS share %s already deleted (idempotent)", nfsShareID)
	} else {
		klog.Infof("Deleted NFS share %s", nfsShareID)
	}

	// Delete the volume.
	if err := c.volumes.Delete(ctx, volumeID); err != nil {
		if !vergeos.IsNotFoundError(err) {
			return nil, status.Errorf(codes.Internal, "deleting volume %s: %v", volumeID, err)
		}
		klog.Infof("Volume %s already deleted (idempotent)", volumeID)
	} else {
		klog.Infof("Deleted NAS volume %s", volumeID)
	}

	return &csi.DeleteVolumeResponse{}, nil
}

// resolveNASService looks up a NAS service by name. If not found and vmID > 0,
// it creates the NAS service on the specified VM (StorageClass "nasServiceVM").
//
// The VergeOS API accepts a "name" on POST /vm_services, but govergeos v0.1.9's
// NASServiceCreateRequest has no Name field, so the service name is derived by
// VergeOS from the VM name. To stay idempotent and never create duplicates:
//   - creation is serialized with a mutex and re-checks by name and by VM
//     before creating;
//   - the resulting service name must equal the requested name, otherwise the
//     call fails with FailedPrecondition (name the VM after nasServiceName).
func (c *NASController) resolveNASService(ctx context.Context, name string, vmID int) (*vergeos.NASService, error) {
	nasSvc, err := c.nasServices.GetByName(ctx, name)
	if err == nil {
		return nasSvc, nil
	}
	if !vergeos.IsNotFoundError(err) {
		return nil, status.Errorf(codes.Internal, "looking up NAS service %q: %v", name, err)
	}
	if vmID <= 0 {
		return nil, status.Errorf(codes.NotFound, "NAS service %q not found; create it in VergeOS or set 'nasServiceVM' in StorageClass parameters", name)
	}

	c.nasServiceMu.Lock()
	defer c.nasServiceMu.Unlock()

	// Another CreateVolume may have created the service while we waited.
	nasSvc, err = c.nasServices.GetByName(ctx, name)
	if err == nil {
		return nasSvc, nil
	}
	if !vergeos.IsNotFoundError(err) {
		return nil, status.Errorf(codes.Internal, "looking up NAS service %q: %v", name, err)
	}

	// A service may already exist on this VM under a different name (e.g. a
	// previous attempt); never create a second one.
	existing, err := c.nasServices.GetByVM(ctx, vmID)
	if err != nil && !vergeos.IsNotFoundError(err) {
		return nil, status.Errorf(codes.Internal, "looking up NAS service on VM %d: %v", vmID, err)
	}
	if existing != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"VM %d already hosts NAS service %q (id=%d), not %q: set StorageClass 'nasServiceName' to %q or rename the VM",
			vmID, existing.Name, existing.Key.Int(), name, existing.Name)
	}

	klog.Infof("NAS service %q not found, creating on demand (VM %d)", name, vmID)
	nasSvc, err = c.nasServices.Create(ctx, &vergeos.NASServiceCreateRequest{VM: vmID})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "auto-creating NAS service %q on VM %d: %v", name, vmID, err)
	}
	klog.Infof("Created NAS service %q (id=%d) on VM %d", nasSvc.Name, nasSvc.Key.Int(), vmID)
	if nasSvc.Name != name {
		return nil, status.Errorf(codes.FailedPrecondition,
			"created NAS service on VM %d is named %q (derived from the VM name), not the requested %q: set StorageClass 'nasServiceName' to %q or rename the VM",
			vmID, nasSvc.Name, name, nasSvc.Name)
	}
	return nasSvc, nil
}

// getNASServiceIP resolves the NAS service's network IP address.
// It tries two strategies in order:
//  1. Guest agent: query machine status for agent_guest_info IPv4 addresses
//     (works for any NAS VM with the VergeOS guest agent installed)
//  2. NIC lookup: resolve VM $key → machine ID, list NICs for ipaddress field
//     (works only when VergeOS manages DHCP internally and populates ipaddress)
func (c *NASController) getNASServiceIP(ctx context.Context, nasSvc *vergeos.NASService) (string, error) {
	vmKey := nasSvc.VM.Int()
	if vmKey == 0 {
		return "", fmt.Errorf("NAS service %q has no VM ID", nasSvc.Name)
	}

	// Resolve VM $key → machine ID (needed for both strategies).
	vm, err := c.vms.Get(ctx, vmKey)
	if err != nil {
		return "", fmt.Errorf("resolving NAS VM %d: %w", vmKey, err)
	}

	// Strategy 1: Guest agent IP (preferred — works regardless of DHCP source).
	if c.machineStatus != nil {
		status, err := c.machineStatus.Get(ctx, vm.Machine)
		if err == nil && status.AgentGuestInfo != nil {
			for _, iface := range status.AgentGuestInfo.Network {
				for _, addr := range iface.IPAddresses {
					if addr.Type == "ipv4" && addr.Address != "" && addr.Address != "127.0.0.1" {
						klog.Infof("Resolved NAS service %q IP via guest agent: %s (VM %d, machine %d)", nasSvc.Name, addr.Address, vmKey, vm.Machine)
						return addr.Address, nil
					}
				}
			}
		}
		if err != nil {
			klog.V(2).Infof("Guest agent IP lookup failed for NAS VM %d (machine %d): %v, trying NIC lookup", vmKey, vm.Machine, err)
		} else {
			klog.V(2).Infof("No IPv4 in guest agent info for NAS VM %d (machine %d), trying NIC lookup", vmKey, vm.Machine)
		}
	}

	// Strategy 2: NIC lookup fallback (for VergeOS-managed DHCP environments).
	nics, err := c.vmNICs.List(ctx, vm.Machine)
	if err != nil {
		return "", fmt.Errorf("listing NICs for NAS VM %d (machine %d): %w", vmKey, vm.Machine, err)
	}

	for _, nic := range nics {
		if nic.IPAddress != "" {
			klog.Infof("Resolved NAS service %q IP via NIC: %s (VM %d, machine %d)", nasSvc.Name, nic.IPAddress, vmKey, vm.Machine)
			return nic.IPAddress, nil
		}
	}

	return "", fmt.Errorf("no IP address found for NAS VM %d (tried guest agent and NIC lookup)", vmKey)
}

// ControllerPublishVolume is not needed for NAS (attachRequired=false).
func (c *NASController) ControllerPublishVolume(_ context.Context, _ *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NAS driver does not implement ControllerPublishVolume")
}

// ControllerUnpublishVolume is not needed for NAS (attachRequired=false).
func (c *NASController) ControllerUnpublishVolume(_ context.Context, _ *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NAS driver does not implement ControllerUnpublishVolume")
}

func (c *NASController) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	// NAS supports RWO, ROX, and RWX.
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.VolumeCapabilities,
		},
	}, nil
}

func (c *NASController) ControllerGetCapabilities() []*csi.ControllerServiceCapability {
	return []*csi.ControllerServiceCapability{
		{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
				},
			},
		},
	}
}
