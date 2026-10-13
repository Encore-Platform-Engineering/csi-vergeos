package nas

import (
	"context"

	vergeos "github.com/verge-io/govergeos"
)

// NASServiceClient abstracts the govergeos NASServices API for testing.
type NASServiceClient interface {
	Get(ctx context.Context, id int) (*vergeos.NASService, error)
	GetByName(ctx context.Context, name string) (*vergeos.NASService, error)
	GetByVM(ctx context.Context, vmID int) (*vergeos.NASService, error)
	Create(ctx context.Context, req *vergeos.NASServiceCreateRequest) (*vergeos.NASService, error)
	Delete(ctx context.Context, id int) error
}

// VolumeClient abstracts the govergeos Volumes API for testing.
type VolumeClient interface {
	Get(ctx context.Context, id string) (*vergeos.Volume, error)
	GetByName(ctx context.Context, serviceID int, name string) (*vergeos.Volume, error)
	Create(ctx context.Context, req *vergeos.VolumeCreateRequest) (*vergeos.Volume, error)
	Delete(ctx context.Context, id string) error
}

// NFSShareClient abstracts the govergeos VolumeNFSShares API for testing.
type NFSShareClient interface {
	Get(ctx context.Context, id string) (*vergeos.VolumeNFSShare, error)
	GetByName(ctx context.Context, volumeID, name string) (*vergeos.VolumeNFSShare, error)
	Create(ctx context.Context, req *vergeos.VolumeNFSShareCreateRequest) (*vergeos.VolumeNFSShare, error)
	Delete(ctx context.Context, id string) error
}

// VMNICClient abstracts the govergeos VMNICs API for looking up NAS service IPs.
type VMNICClient interface {
	List(ctx context.Context, vmID int) ([]vergeos.VMNIC, error)
	Get(ctx context.Context, nicID int) (*vergeos.VMNIC, error)
}

// VMClient abstracts the govergeos VMs API for resolving VM machine IDs.
type VMClient interface {
	Get(ctx context.Context, id int) (*vergeos.VM, error)
}

// MachineStatusClient abstracts the govergeos MachineStatus API for guest agent lookups.
type MachineStatusClient interface {
	Get(ctx context.Context, machineKey int) (*vergeos.MachineStatus, error)
}
