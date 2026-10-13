package nas

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	vergeos "github.com/verge-io/govergeos"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// --- Mocks ---

type mockNASServiceClient struct {
	getByNameFn func(ctx context.Context, name string) (*vergeos.NASService, error)
	getByVMFn   func(ctx context.Context, vmID int) (*vergeos.NASService, error)
	createFn    func(ctx context.Context, req *vergeos.NASServiceCreateRequest) (*vergeos.NASService, error)
}

func (m *mockNASServiceClient) Get(_ context.Context, id int) (*vergeos.NASService, error) {
	return &vergeos.NASService{Key: vergeos.FlexInt(id), Name: "test-nas", VM: 100}, nil
}
func (m *mockNASServiceClient) GetByName(ctx context.Context, name string) (*vergeos.NASService, error) {
	if m.getByNameFn != nil {
		return m.getByNameFn(ctx, name)
	}
	return nil, &vergeos.NotFoundError{Resource: "NASService", ID: name}
}
func (m *mockNASServiceClient) GetByVM(ctx context.Context, vmID int) (*vergeos.NASService, error) {
	if m.getByVMFn != nil {
		return m.getByVMFn(ctx, vmID)
	}
	return nil, &vergeos.NotFoundError{Resource: "NASService", ID: vmID}
}
func (m *mockNASServiceClient) Create(ctx context.Context, req *vergeos.NASServiceCreateRequest) (*vergeos.NASService, error) {
	if m.createFn != nil {
		return m.createFn(ctx, req)
	}
	return &vergeos.NASService{Key: 1, Name: "test-nas", VM: 100}, nil
}
func (m *mockNASServiceClient) Delete(_ context.Context, _ int) error { return nil }

type mockVolumeClient struct {
	getByNameFn func(ctx context.Context, serviceID int, name string) (*vergeos.Volume, error)
	createFn    func(ctx context.Context, req *vergeos.VolumeCreateRequest) (*vergeos.Volume, error)
	deleteFn    func(ctx context.Context, id string) error
}

func (m *mockVolumeClient) Get(_ context.Context, id string) (*vergeos.Volume, error) {
	return &vergeos.Volume{ID: id}, nil
}
func (m *mockVolumeClient) GetByName(ctx context.Context, serviceID int, name string) (*vergeos.Volume, error) {
	if m.getByNameFn != nil {
		return m.getByNameFn(ctx, serviceID, name)
	}
	return nil, &vergeos.NotFoundError{Resource: "Volume", ID: name}
}
func (m *mockVolumeClient) Create(ctx context.Context, req *vergeos.VolumeCreateRequest) (*vergeos.Volume, error) {
	if m.createFn != nil {
		return m.createFn(ctx, req)
	}
	return &vergeos.Volume{ID: "sha1vol", Name: req.Name, MaxSize: 1073741824}, nil
}
func (m *mockVolumeClient) Delete(ctx context.Context, id string) error {
	if m.deleteFn != nil {
		return m.deleteFn(ctx, id)
	}
	return nil
}

type mockNFSShareClient struct {
	getByNameFn func(ctx context.Context, volumeID, name string) (*vergeos.VolumeNFSShare, error)
	createFn    func(ctx context.Context, req *vergeos.VolumeNFSShareCreateRequest) (*vergeos.VolumeNFSShare, error)
	deleteFn    func(ctx context.Context, id string) error
}

func (m *mockNFSShareClient) Get(_ context.Context, id string) (*vergeos.VolumeNFSShare, error) {
	return &vergeos.VolumeNFSShare{ID: id}, nil
}
func (m *mockNFSShareClient) GetByName(ctx context.Context, volumeID, name string) (*vergeos.VolumeNFSShare, error) {
	if m.getByNameFn != nil {
		return m.getByNameFn(ctx, volumeID, name)
	}
	return nil, &vergeos.NotFoundError{Resource: "VolumeNFSShare", ID: name}
}
func (m *mockNFSShareClient) Create(ctx context.Context, req *vergeos.VolumeNFSShareCreateRequest) (*vergeos.VolumeNFSShare, error) {
	if m.createFn != nil {
		return m.createFn(ctx, req)
	}
	return &vergeos.VolumeNFSShare{ID: "sha1share", Name: req.Name, Volume: req.Volume}, nil
}
func (m *mockNFSShareClient) Delete(ctx context.Context, id string) error {
	if m.deleteFn != nil {
		return m.deleteFn(ctx, id)
	}
	return nil
}

type mockVMNICClient struct {
	listFn func(ctx context.Context, vmID int) ([]vergeos.VMNIC, error)
	getFn  func(ctx context.Context, nicID int) (*vergeos.VMNIC, error)
}

func (m *mockVMNICClient) List(ctx context.Context, vmID int) ([]vergeos.VMNIC, error) {
	if m.listFn != nil {
		return m.listFn(ctx, vmID)
	}
	return []vergeos.VMNIC{
		{ID: 1, IPAddress: "10.0.0.5"},
	}, nil
}

func (m *mockVMNICClient) Get(ctx context.Context, nicID int) (*vergeos.VMNIC, error) {
	if m.getFn != nil {
		return m.getFn(ctx, nicID)
	}
	return &vergeos.VMNIC{ID: vergeos.FlexInt(nicID), IPAddress: "10.0.0.5"}, nil
}

type mockVMClient struct {
	getFn func(ctx context.Context, id int) (*vergeos.VM, error)
}

func (m *mockVMClient) Get(ctx context.Context, id int) (*vergeos.VM, error) {
	if m.getFn != nil {
		return m.getFn(ctx, id)
	}
	// Default: VM $key matches id, machine = id + 1000
	return &vergeos.VM{ID: vergeos.FlexInt(id), Name: "nas-vm", Machine: id + 1000}, nil
}

type mockMachineStatusClient struct {
	getFn func(ctx context.Context, machineKey int) (*vergeos.MachineStatus, error)
}

func (m *mockMachineStatusClient) Get(ctx context.Context, machineKey int) (*vergeos.MachineStatus, error) {
	if m.getFn != nil {
		return m.getFn(ctx, machineKey)
	}
	return &vergeos.MachineStatus{
		Machine: machineKey,
		Running: true,
		AgentGuestInfo: &vergeos.GuestInfo{
			Network: []vergeos.GuestNetworkInterface{
				{
					Name: "enp1s1",
					IPAddresses: []vergeos.GuestIPAddress{
						{Type: "ipv4", Address: "10.0.0.5", Prefix: 24},
					},
				},
			},
		},
	}, nil
}

// --- Tests ---

func newTestController() (*NASController, *mockNASServiceClient, *mockVolumeClient, *mockNFSShareClient) {
	nasMock := &mockNASServiceClient{
		getByNameFn: func(_ context.Context, name string) (*vergeos.NASService, error) {
			return &vergeos.NASService{Key: 1, Name: name, VM: 100}, nil
		},
	}
	volMock := &mockVolumeClient{}
	nfsMock := &mockNFSShareClient{}
	nicMock := &mockVMNICClient{}

	c := &NASController{
		nasServices:   nasMock,
		volumes:       volMock,
		nfsShares:     nfsMock,
		vmNICs:        nicMock,
		vms:           &mockVMClient{},
		machineStatus: &mockMachineStatusClient{},
	}
	return c, nasMock, volMock, nfsMock
}

func TestCreateVolume_Success(t *testing.T) {
	c, _, volMock, nfsMock := newTestController()

	volMock.createFn = func(_ context.Context, req *vergeos.VolumeCreateRequest) (*vergeos.Volume, error) {
		if req.Name != "csi-test-pvc" {
			t.Errorf("volume name = %q, want %q", req.Name, "csi-test-pvc")
		}
		return &vergeos.Volume{ID: "vol123", Name: req.Name, MaxSize: 1073741824}, nil
	}
	nfsMock.createFn = func(_ context.Context, req *vergeos.VolumeNFSShareCreateRequest) (*vergeos.VolumeNFSShare, error) {
		return &vergeos.VolumeNFSShare{ID: "share456", Name: req.Name, Volume: req.Volume}, nil
	}

	req := &csi.CreateVolumeRequest{
		Name: "test-pvc",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1073741824, // 1 GiB
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
				},
			},
		},
		Parameters: map[string]string{
			"nasServiceName": "k8s-nas",
			"allowedHosts":   "10.0.0.0/24",
		},
	}

	resp, err := c.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	if resp.Volume == nil {
		t.Fatal("response volume is nil")
	}
	if resp.Volume.VolumeId != "1:vol123:share456" {
		t.Errorf("VolumeId = %q, want %q", resp.Volume.VolumeId, "1:vol123:share456")
	}
	// Verify nasServiceIP is in the volume context.
	if ip := resp.Volume.VolumeContext["nasServiceIP"]; ip != "10.0.0.5" {
		t.Errorf("nasServiceIP = %q, want %q", ip, "10.0.0.5")
	}
}

func TestCreateVolume_StaticIP(t *testing.T) {
	c, _, volMock, nfsMock := newTestController()

	volMock.createFn = func(_ context.Context, req *vergeos.VolumeCreateRequest) (*vergeos.Volume, error) {
		return &vergeos.Volume{ID: "vol123", Name: req.Name, MaxSize: 1073741824}, nil
	}
	nfsMock.createFn = func(_ context.Context, req *vergeos.VolumeNFSShareCreateRequest) (*vergeos.VolumeNFSShare, error) {
		return &vergeos.VolumeNFSShare{ID: "share456", Name: req.Name, Volume: req.Volume}, nil
	}

	req := &csi.CreateVolumeRequest{
		Name: "test-pvc",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1073741824,
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			{AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER}},
		},
		Parameters: map[string]string{
			"nasServiceName": "k8s-nas",
			"allowedHosts":   "10.0.0.0/24",
			"nasServiceIP":   "192.168.1.100",
		},
	}

	resp, err := c.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume with static IP failed: %v", err)
	}
	// Static IP from StorageClass should be used.
	if ip := resp.Volume.VolumeContext["nasServiceIP"]; ip != "192.168.1.100" {
		t.Errorf("nasServiceIP = %q, want %q", ip, "192.168.1.100")
	}
}

func TestCreateVolume_Idempotent(t *testing.T) {
	c, _, volMock, nfsMock := newTestController()

	// Volume already exists
	volMock.getByNameFn = func(_ context.Context, _ int, _ string) (*vergeos.Volume, error) {
		return &vergeos.Volume{ID: "existingvol", Name: "csi-test-pvc", MaxSize: 1073741824}, nil
	}
	// NFS share already exists
	nfsMock.getByNameFn = func(_ context.Context, _, _ string) (*vergeos.VolumeNFSShare, error) {
		return &vergeos.VolumeNFSShare{ID: "existingshare", Volume: "existingvol"}, nil
	}

	req := &csi.CreateVolumeRequest{
		Name: "test-pvc",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1073741824,
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			{AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER}},
		},
		Parameters: map[string]string{"nasServiceName": "k8s-nas", "allowedHosts": "10.0.0.0/24"},
	}

	resp, err := c.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume (idempotent) failed: %v", err)
	}
	if resp.Volume.VolumeId != "1:existingvol:existingshare" {
		t.Errorf("VolumeId = %q, want %q", resp.Volume.VolumeId, "1:existingvol:existingshare")
	}
}

func TestCreateVolume_MissingName(t *testing.T) {
	c, _, _, _ := newTestController()

	req := &csi.CreateVolumeRequest{
		Name: "",
	}
	_, err := c.CreateVolume(context.Background(), req)
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", err)
	}
}

func TestCreateVolume_MissingNASServiceName(t *testing.T) {
	c, _, _, _ := newTestController()

	req := &csi.CreateVolumeRequest{
		Name: "test-pvc",
		VolumeCapabilities: []*csi.VolumeCapability{
			{AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER}},
		},
		Parameters: map[string]string{},
	}
	_, err := c.CreateVolume(context.Background(), req)
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for missing nasServiceName, got %v", err)
	}
}

func TestDeleteVolume_Success(t *testing.T) {
	c, _, _, _ := newTestController()

	req := &csi.DeleteVolumeRequest{
		VolumeId: "1:vol123:share456",
	}
	_, err := c.DeleteVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("DeleteVolume failed: %v", err)
	}
}

func TestDeleteVolume_AlreadyGone(t *testing.T) {
	c, _, volMock, nfsMock := newTestController()

	nfsMock.deleteFn = func(_ context.Context, _ string) error {
		return &vergeos.NotFoundError{Resource: "VolumeNFSShare", ID: "share456"}
	}
	volMock.deleteFn = func(_ context.Context, _ string) error {
		return &vergeos.NotFoundError{Resource: "Volume", ID: "vol123"}
	}

	req := &csi.DeleteVolumeRequest{
		VolumeId: "1:vol123:share456",
	}
	_, err := c.DeleteVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("DeleteVolume (already gone) should succeed: %v", err)
	}
}

func TestDeleteVolume_InvalidVolumeID(t *testing.T) {
	c, _, _, _ := newTestController()

	req := &csi.DeleteVolumeRequest{VolumeId: "invalid"}
	_, err := c.DeleteVolume(context.Background(), req)
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", err)
	}
}

func TestDeleteVolume_APIError(t *testing.T) {
	c, _, _, nfsMock := newTestController()

	nfsMock.deleteFn = func(_ context.Context, _ string) error {
		return fmt.Errorf("API connection failed")
	}

	req := &csi.DeleteVolumeRequest{VolumeId: "1:vol123:share456"}
	_, err := c.DeleteVolume(context.Background(), req)
	if err == nil {
		t.Fatal("expected error on API failure")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Internal {
		t.Errorf("expected Internal error, got %v", err)
	}
}

func TestNASCreateVolume_AutoCreateService(t *testing.T) {
	// Test that when a NAS service doesn't exist and nasServiceVM is provided,
	// the controller auto-creates the NAS service.
	nasMock := &mockNASServiceClient{
		getByNameFn: func(_ context.Context, _ string) (*vergeos.NASService, error) {
			return nil, &vergeos.NotFoundError{Resource: "NASService", ID: "new-nas"}
		},
		createFn: func(_ context.Context, req *vergeos.NASServiceCreateRequest) (*vergeos.NASService, error) {
			if req.VM != 200 {
				t.Errorf("expected VM=200 in create request, got %d", req.VM)
			}
			return &vergeos.NASService{Key: 10, Name: "new-nas", VM: 200}, nil
		},
	}
	volMock := &mockVolumeClient{
		createFn: func(_ context.Context, req *vergeos.VolumeCreateRequest) (*vergeos.Volume, error) {
			return &vergeos.Volume{ID: "vol999", Name: req.Name, MaxSize: 1073741824}, nil
		},
	}
	nfsMock := &mockNFSShareClient{
		createFn: func(_ context.Context, req *vergeos.VolumeNFSShareCreateRequest) (*vergeos.VolumeNFSShare, error) {
			return &vergeos.VolumeNFSShare{ID: "share999", Name: req.Name, Volume: req.Volume}, nil
		},
	}
	nicMock := &mockVMNICClient{
		listFn: func(_ context.Context, _ int) ([]vergeos.VMNIC, error) {
			return []vergeos.VMNIC{{ID: 1, IPAddress: "10.0.0.20"}}, nil
		},
	}

	c := &NASController{
		nasServices:   nasMock,
		volumes:       volMock,
		nfsShares:     nfsMock,
		vmNICs:        nicMock,
		vms:           &mockVMClient{},
		machineStatus: &mockMachineStatusClient{},
	}

	req := &csi.CreateVolumeRequest{
		Name: "auto-pvc",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1073741824,
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			{AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER}},
		},
		Parameters: map[string]string{
			"nasServiceName": "new-nas",
			"allowedHosts":   "10.0.0.0/24",
			"nasServiceVM":   "200",
		},
	}

	resp, err := c.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume with auto-create service failed: %v", err)
	}
	if resp.Volume == nil {
		t.Fatal("response volume is nil")
	}
	if resp.Volume.VolumeId != "10:vol999:share999" {
		t.Errorf("VolumeId = %q, want %q", resp.Volume.VolumeId, "10:vol999:share999")
	}
}

func TestNASCreateVolume_AutoCreateService_NoVM(t *testing.T) {
	// Test that when a NAS service doesn't exist and no nasServiceVM is provided,
	// the controller returns a helpful NotFound error.
	nasMock := &mockNASServiceClient{
		getByNameFn: func(_ context.Context, _ string) (*vergeos.NASService, error) {
			return nil, &vergeos.NotFoundError{Resource: "NASService", ID: "missing-nas"}
		},
	}
	volMock := &mockVolumeClient{}
	nfsMock := &mockNFSShareClient{}
	nicMock := &mockVMNICClient{}

	c := &NASController{
		nasServices:   nasMock,
		volumes:       volMock,
		nfsShares:     nfsMock,
		vmNICs:        nicMock,
		vms:           &mockVMClient{},
		machineStatus: &mockMachineStatusClient{},
	}

	req := &csi.CreateVolumeRequest{
		Name: "test-pvc",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1073741824,
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			{AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER}},
		},
		Parameters: map[string]string{
			"nasServiceName": "missing-nas",
			"allowedHosts":   "10.0.0.0/24",
			// No nasServiceVM — should fail with helpful error
		},
	}

	_, err := c.CreateVolume(context.Background(), req)
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.NotFound {
		t.Errorf("expected NotFound error, got %v", err)
	}
	// Verify the error message is helpful
	if st.Message() == "" {
		t.Error("expected helpful error message")
	}
}

func TestNASCreateVolume_AutoCreateService_CreateFails(t *testing.T) {
	// Test that when auto-create fails, the error is propagated.
	nasMock := &mockNASServiceClient{
		getByNameFn: func(_ context.Context, _ string) (*vergeos.NASService, error) {
			return nil, &vergeos.NotFoundError{Resource: "NASService", ID: "new-nas"}
		},
		createFn: func(_ context.Context, _ *vergeos.NASServiceCreateRequest) (*vergeos.NASService, error) {
			return nil, fmt.Errorf("API error: insufficient resources")
		},
	}
	volMock := &mockVolumeClient{}
	nfsMock := &mockNFSShareClient{}
	nicMock := &mockVMNICClient{}

	c := &NASController{
		nasServices:   nasMock,
		volumes:       volMock,
		nfsShares:     nfsMock,
		vmNICs:        nicMock,
		vms:           &mockVMClient{},
		machineStatus: &mockMachineStatusClient{},
	}

	req := &csi.CreateVolumeRequest{
		Name: "test-pvc",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1073741824,
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			{AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER}},
		},
		Parameters: map[string]string{
			"nasServiceName": "new-nas",
			"allowedHosts":   "10.0.0.0/24",
			"nasServiceVM":   "200",
		},
	}

	_, err := c.CreateVolume(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when auto-create fails")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Internal {
		t.Errorf("expected Internal error, got %v", err)
	}
}

// --- NFS export policy (#12) ---

func createReqWithParams(params map[string]string) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name:          "test-pvc",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1073741824},
		VolumeCapabilities: []*csi.VolumeCapability{
			{AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER}},
		},
		Parameters: params,
	}
}

// captureShareCreate wires nfsMock to record the share create request.
func captureShareCreate(nfsMock *mockNFSShareClient, got **vergeos.VolumeNFSShareCreateRequest) {
	nfsMock.createFn = func(_ context.Context, req *vergeos.VolumeNFSShareCreateRequest) (*vergeos.VolumeNFSShare, error) {
		*got = req
		return &vergeos.VolumeNFSShare{ID: "share456", Name: req.Name, Volume: req.Volume}, nil
	}
}

func TestCreateVolume_ShareRestrictedToAllowedHosts(t *testing.T) {
	c, _, _, nfsMock := newTestController()
	var got *vergeos.VolumeNFSShareCreateRequest
	captureShareCreate(nfsMock, &got)

	_, err := c.CreateVolume(context.Background(), createReqWithParams(map[string]string{
		"nasServiceName": "k8s-nas",
		"allowedHosts":   " 10.0.0.0/24 , 192.168.1.7 ,",
	}))
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if got == nil {
		t.Fatal("share was not created")
	}
	if got.AllowAll == nil || *got.AllowAll {
		t.Errorf("AllowAll = %v, want false", got.AllowAll)
	}
	if got.AllowedHosts == nil || *got.AllowedHosts != "10.0.0.0/24,192.168.1.7" {
		t.Errorf("AllowedHosts = %v, want 10.0.0.0/24,192.168.1.7", got.AllowedHosts)
	}
	if got.Squash == nil || *got.Squash != "root_squash" {
		t.Errorf("Squash = %v, want root_squash default", got.Squash)
	}
}

func TestCreateVolume_SquashOptIn(t *testing.T) {
	c, _, _, nfsMock := newTestController()
	var got *vergeos.VolumeNFSShareCreateRequest
	captureShareCreate(nfsMock, &got)

	_, err := c.CreateVolume(context.Background(), createReqWithParams(map[string]string{
		"nasServiceName": "k8s-nas",
		"allowedHosts":   "10.0.0.0/24",
		"squash":         "no_root_squash",
	}))
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if got.Squash == nil || *got.Squash != "no_root_squash" {
		t.Errorf("Squash = %v, want no_root_squash", got.Squash)
	}
}

func TestCreateVolume_ExplicitAllowAll(t *testing.T) {
	c, _, _, nfsMock := newTestController()
	var got *vergeos.VolumeNFSShareCreateRequest
	captureShareCreate(nfsMock, &got)

	_, err := c.CreateVolume(context.Background(), createReqWithParams(map[string]string{
		"nasServiceName": "k8s-nas",
		"allowAll":       "true",
	}))
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if got.AllowAll == nil || !*got.AllowAll {
		t.Errorf("AllowAll = %v, want true", got.AllowAll)
	}
	if got.AllowedHosts != nil {
		t.Errorf("AllowedHosts = %q, want unset with allowAll", *got.AllowedHosts)
	}
	if got.Squash == nil || *got.Squash != "root_squash" {
		t.Errorf("Squash = %v, want root_squash even with allowAll", got.Squash)
	}
}

func TestCreateVolume_InvalidExportParams(t *testing.T) {
	base := func(extra map[string]string) map[string]string {
		p := map[string]string{"nasServiceName": "k8s-nas"}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	for name, params := range map[string]map[string]string{
		"neither allowedHosts nor allowAll": base(nil),
		"allowAll false and no hosts":       base(map[string]string{"allowAll": "false"}),
		"only separators in hosts":          base(map[string]string{"allowedHosts": " , "}),
		"bad host entry":                    base(map[string]string{"allowedHosts": "10.0.0.0/24,not-an-ip"}),
		"bad CIDR":                          base(map[string]string{"allowedHosts": "10.0.0.0/99"}),
		"match-all IPv4 CIDR":               base(map[string]string{"allowedHosts": "10.0.0.0/24,0.0.0.0/0"}),
		"match-all IPv6 CIDR":               base(map[string]string{"allowedHosts": "::/0"}),
		"bad allowAll value":                base(map[string]string{"allowAll": "yes"}),
		"allowAll with hosts":               base(map[string]string{"allowAll": "true", "allowedHosts": "10.0.0.0/24"}),
		"bad squash":                        base(map[string]string{"allowedHosts": "10.0.0.0/24", "squash": "none"}),
	} {
		t.Run(name, func(t *testing.T) {
			c, nasMock, volMock, nfsMock := newTestController()
			calls := 0
			nasMock.getByNameFn = func(_ context.Context, _ string) (*vergeos.NASService, error) {
				calls++
				return &vergeos.NASService{Key: 1, Name: "k8s-nas"}, nil
			}
			volMock.createFn = func(_ context.Context, _ *vergeos.VolumeCreateRequest) (*vergeos.Volume, error) {
				calls++
				return &vergeos.Volume{ID: "v"}, nil
			}
			nfsMock.createFn = func(_ context.Context, _ *vergeos.VolumeNFSShareCreateRequest) (*vergeos.VolumeNFSShare, error) {
				calls++
				return &vergeos.VolumeNFSShare{ID: "s"}, nil
			}
			_, err := c.CreateVolume(context.Background(), createReqWithParams(params))
			if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument {
				t.Fatalf("code = %v, want InvalidArgument (err=%v)", st.Code(), err)
			}
			if calls != 0 {
				t.Errorf("API was called %d times before parameter validation failed", calls)
			}
		})
	}
}

func TestCreateVolume_ExistingShareNotAltered(t *testing.T) {
	c, _, volMock, nfsMock := newTestController()
	volMock.getByNameFn = func(_ context.Context, _ int, _ string) (*vergeos.Volume, error) {
		return &vergeos.Volume{ID: "existingvol", MaxSize: 1073741824}, nil
	}
	nfsMock.getByNameFn = func(_ context.Context, _, _ string) (*vergeos.VolumeNFSShare, error) {
		return &vergeos.VolumeNFSShare{ID: "existingshare", Volume: "existingvol"}, nil
	}
	nfsMock.createFn = func(_ context.Context, _ *vergeos.VolumeNFSShareCreateRequest) (*vergeos.VolumeNFSShare, error) {
		t.Error("existing share must not be re-created or altered")
		return nil, fmt.Errorf("unexpected")
	}
	if _, err := c.CreateVolume(context.Background(), createReqWithParams(map[string]string{
		"nasServiceName": "k8s-nas", "allowedHosts": "10.0.0.0/24",
	})); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
}

// --- On-demand NAS service creation (#13) ---

func autoCreateController(nasMock *mockNASServiceClient) *NASController {
	return &NASController{
		nasServices:   nasMock,
		volumes:       &mockVolumeClient{},
		nfsShares:     &mockNFSShareClient{},
		vmNICs:        &mockVMNICClient{},
		vms:           &mockVMClient{},
		machineStatus: &mockMachineStatusClient{},
	}
}

func TestNASCreateVolume_AutoCreate_NameMismatchFailsWithoutRepeatedCreates(t *testing.T) {
	// VergeOS derives the service name from the VM name; govergeos cannot set it.
	// The created service is therefore named "vm-200", not "k8s-nas".
	var svcOnVM *vergeos.NASService
	creates := 0
	nasMock := &mockNASServiceClient{
		getByNameFn: func(_ context.Context, name string) (*vergeos.NASService, error) {
			if svcOnVM != nil && svcOnVM.Name == name {
				return svcOnVM, nil
			}
			return nil, &vergeos.NotFoundError{Resource: "NASService", ID: name}
		},
		getByVMFn: func(_ context.Context, _ int) (*vergeos.NASService, error) {
			if svcOnVM != nil {
				return svcOnVM, nil
			}
			return nil, &vergeos.NotFoundError{Resource: "NASService", ID: 200}
		},
		createFn: func(_ context.Context, _ *vergeos.NASServiceCreateRequest) (*vergeos.NASService, error) {
			creates++
			svcOnVM = &vergeos.NASService{Key: 10, Name: "vm-200", VM: 200}
			return svcOnVM, nil
		},
	}
	c := autoCreateController(nasMock)
	req := createReqWithParams(map[string]string{
		"nasServiceName": "k8s-nas", "nasServiceVM": "200", "allowedHosts": "10.0.0.0/24",
	})

	for i := 0; i < 3; i++ {
		_, err := c.CreateVolume(context.Background(), req)
		if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition {
			t.Fatalf("attempt %d: code = %v, want FailedPrecondition (err=%v)", i, st.Code(), err)
		}
	}
	if creates != 1 {
		t.Errorf("NAS service creates = %d, want exactly 1 across retries", creates)
	}
}

func TestNASCreateVolume_AutoCreate_ReusesServiceAlreadyOnVM(t *testing.T) {
	nasMock := &mockNASServiceClient{
		getByNameFn: func(_ context.Context, name string) (*vergeos.NASService, error) {
			return nil, &vergeos.NotFoundError{Resource: "NASService", ID: name}
		},
		getByVMFn: func(_ context.Context, _ int) (*vergeos.NASService, error) {
			return &vergeos.NASService{Key: 10, Name: "other", VM: 200}, nil
		},
		createFn: func(_ context.Context, _ *vergeos.NASServiceCreateRequest) (*vergeos.NASService, error) {
			t.Error("must not create a second NAS service on a VM that already has one")
			return nil, fmt.Errorf("unexpected")
		},
	}
	_, err := autoCreateController(nasMock).CreateVolume(context.Background(), createReqWithParams(map[string]string{
		"nasServiceName": "k8s-nas", "nasServiceVM": "200", "allowedHosts": "10.0.0.0/24",
	}))
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", st.Code(), err)
	}
}

func TestNASCreateVolume_AutoCreate_ConcurrentCallsCreateOnce(t *testing.T) {
	var (
		mu      sync.Mutex
		created *vergeos.NASService
		creates int
	)
	nasMock := &mockNASServiceClient{
		getByNameFn: func(_ context.Context, name string) (*vergeos.NASService, error) {
			mu.Lock()
			defer mu.Unlock()
			if created != nil {
				return created, nil
			}
			return nil, &vergeos.NotFoundError{Resource: "NASService", ID: name}
		},
		createFn: func(_ context.Context, _ *vergeos.NASServiceCreateRequest) (*vergeos.NASService, error) {
			time.Sleep(20 * time.Millisecond) // widen the race window
			mu.Lock()
			defer mu.Unlock()
			creates++
			created = &vergeos.NASService{Key: 10, Name: "k8s-nas", VM: 200}
			return created, nil
		},
	}
	c := autoCreateController(nasMock)
	req := createReqWithParams(map[string]string{
		"nasServiceName": "k8s-nas", "nasServiceVM": "200", "allowedHosts": "10.0.0.0/24", "nasServiceIP": "10.0.0.9",
	})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.CreateVolume(context.Background(), req); err != nil {
				t.Errorf("CreateVolume: %v", err)
			}
		}()
	}
	wg.Wait()
	if creates != 1 {
		t.Errorf("NAS service creates = %d, want 1", creates)
	}
}

func TestCreateVolume_BlockCapabilityRejected(t *testing.T) {
	c, _, volMock, _ := newTestController()
	volMock.createFn = func(_ context.Context, _ *vergeos.VolumeCreateRequest) (*vergeos.Volume, error) {
		t.Error("no volume may be created for a block-mode capability")
		return nil, fmt.Errorf("unexpected")
	}
	req := createReqWithParams(map[string]string{"nasServiceName": "k8s-nas", "allowedHosts": "10.0.0.0/24"})
	req.VolumeCapabilities = []*csi.VolumeCapability{{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
	}}
	_, err := c.CreateVolume(context.Background(), req)
	if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err=%v)", st.Code(), err)
	}
}
