package block

import (
	"context"
	"fmt"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	vergeos "github.com/verge-io/govergeos"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// --- Mocks ---

type mockVMDriveClient struct {
	drives   map[int]*vergeos.VMDrive
	listFn   func(ctx context.Context, vmID int) ([]vergeos.VMDrive, error)
	createFn func(ctx context.Context, vmID int, req *vergeos.VMDriveCreateRequest) (*vergeos.VMDrive, error)
	deleteFn func(ctx context.Context, driveID int) error
	updateFn func(ctx context.Context, driveID int, req *vergeos.VMDriveUpdateRequest) (*vergeos.VMDrive, error)
}

func newMockVMDriveClient() *mockVMDriveClient {
	return &mockVMDriveClient{drives: make(map[int]*vergeos.VMDrive)}
}

func (m *mockVMDriveClient) List(ctx context.Context, vmID int) ([]vergeos.VMDrive, error) {
	if m.listFn != nil {
		return m.listFn(ctx, vmID)
	}
	return nil, nil
}

func (m *mockVMDriveClient) Get(_ context.Context, driveID int) (*vergeos.VMDrive, error) {
	d, ok := m.drives[driveID]
	if !ok {
		return nil, &vergeos.NotFoundError{Resource: "VMDrive", ID: driveID}
	}
	return d, nil
}

func (m *mockVMDriveClient) Create(ctx context.Context, vmID int, req *vergeos.VMDriveCreateRequest) (*vergeos.VMDrive, error) {
	if m.createFn != nil {
		return m.createFn(ctx, vmID, req)
	}
	drive := &vergeos.VMDrive{
		ID:        vergeos.FlexInt(100),
		Name:      req.Name,
		Serial:    req.Serial,
		SizeBytes: req.SizeBytes,
		Machine:   vmID,
	}
	m.drives[100] = drive
	return drive, nil
}

func (m *mockVMDriveClient) Update(ctx context.Context, driveID int, req *vergeos.VMDriveUpdateRequest) (*vergeos.VMDrive, error) {
	if m.updateFn != nil {
		return m.updateFn(ctx, driveID, req)
	}
	d, ok := m.drives[driveID]
	if !ok {
		return nil, &vergeos.NotFoundError{Resource: "VMDrive", ID: driveID}
	}
	// Simulate Machine field update (used for drive move operations).
	if req.Machine != nil {
		d.Machine = *req.Machine
	}
	return d, nil
}

func (m *mockVMDriveClient) Delete(ctx context.Context, driveID int) error {
	if m.deleteFn != nil {
		return m.deleteFn(ctx, driveID)
	}
	delete(m.drives, driveID)
	return nil
}

func (m *mockVMDriveClient) HotplugDrive(_ context.Context, _, _ int) error {
	return nil
}

func (m *mockVMDriveClient) HotUnplugDrive(_ context.Context, _, _ int) error {
	return nil
}

type mockVMClient struct {
	vms    map[int]*vergeos.VM
	listFn func(ctx context.Context, opts ...vergeos.ListOption) ([]vergeos.VM, error)
}

func newMockVMClient() *mockVMClient {
	return &mockVMClient{vms: make(map[int]*vergeos.VM)}
}

func (m *mockVMClient) Get(_ context.Context, id int) (*vergeos.VM, error) {
	if m.vms != nil {
		if vm, ok := m.vms[id]; ok {
			return vm, nil
		}
	}
	return nil, &vergeos.NotFoundError{Resource: "VM", ID: id}
}

func (m *mockVMClient) List(ctx context.Context, opts ...vergeos.ListOption) ([]vergeos.VM, error) {
	if m.listFn != nil {
		return m.listFn(ctx, opts...)
	}
	// Return all VMs from the map.
	var result []vergeos.VM
	for _, vm := range m.vms {
		result = append(result, *vm)
	}
	return result, nil
}

// --- Tests ---

func newTestBlockController() (*BlockController, *mockVMDriveClient) {
	driveMock := newMockVMDriveClient()
	vmMock := newMockVMClient()
	// Populate with a known VM that matches the default test node ID.
	// VM $key=50, internal machine ID=500.
	vmMock.vms[50] = &vergeos.VM{ID: 50, Name: "k8s-node-1", Machine: 500}
	c := &BlockController{
		vmDrives:      driveMock,
		vms:           vmMock,
		poolVMID:      1,  // storage pool VM $key
		poolMachineID: 10, // storage pool VM internal machine ID
	}
	return c, driveMock
}

func newTestBlockControllerWithVMs() (*BlockController, *mockVMDriveClient, *mockVMClient) {
	driveMock := newMockVMDriveClient()
	vmMock := newMockVMClient()
	c := &BlockController{
		vmDrives:      driveMock,
		vms:           vmMock,
		poolVMID:      1,  // storage pool VM $key
		poolMachineID: 10, // storage pool VM internal machine ID
	}
	return c, driveMock, vmMock
}

func TestBlockCreateVolume_Success(t *testing.T) {
	c, driveMock := newTestBlockController()

	driveMock.createFn = func(_ context.Context, vmID int, req *vergeos.VMDriveCreateRequest) (*vergeos.VMDrive, error) {
		if vmID != 10 {
			t.Errorf("expected pool machine ID 10, got %d", vmID)
		}
		if req.Name != "csi-test-pvc" {
			t.Errorf("drive name = %q, want %q", req.Name, "csi-test-pvc")
		}
		if req.Serial == "" {
			t.Error("serial must be set")
		}
		return &vergeos.VMDrive{
			ID:        100,
			Name:      req.Name,
			Serial:    req.Serial,
			SizeBytes: req.SizeBytes,
		}, nil
	}

	req := &csi.CreateVolumeRequest{
		Name: "test-pvc",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 10737418240, // 10 GiB
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
			},
		},
		Parameters: map[string]string{},
	}

	resp, err := c.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	if resp.Volume.VolumeId != "100" {
		t.Errorf("VolumeId = %q, want %q", resp.Volume.VolumeId, "100")
	}
}

func TestBlockCreateVolume_Idempotent(t *testing.T) {
	c, driveMock := newTestBlockController()

	// Drive already exists on the pool VM.
	driveMock.listFn = func(_ context.Context, vmID int) ([]vergeos.VMDrive, error) {
		return []vergeos.VMDrive{
			{ID: 100, Name: "csi-test-pvc", Serial: "csi-abc123", SizeBytes: 10737418240},
		}, nil
	}

	req := &csi.CreateVolumeRequest{
		Name: "test-pvc",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 10737418240,
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			{AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}},
		},
	}

	resp, err := c.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume (idempotent) failed: %v", err)
	}
	if resp.Volume.VolumeId != "100" {
		t.Errorf("VolumeId = %q, want %q", resp.Volume.VolumeId, "100")
	}
}

func TestBlockCreateVolume_MissingName(t *testing.T) {
	c, _ := newTestBlockController()
	_, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{Name: ""})
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", err)
	}
}

func TestBlockDeleteVolume_Success(t *testing.T) {
	c, driveMock := newTestBlockController()
	driveMock.drives[100] = &vergeos.VMDrive{ID: 100, Name: "csi-test-pvc"}

	_, err := c.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "100"})
	if err != nil {
		t.Fatalf("DeleteVolume failed: %v", err)
	}
}

func TestBlockDeleteVolume_AlreadyGone(t *testing.T) {
	c, driveMock := newTestBlockController()
	driveMock.deleteFn = func(_ context.Context, _ int) error {
		return &vergeos.NotFoundError{Resource: "VMDrive", ID: 100}
	}

	_, err := c.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "100"})
	if err != nil {
		t.Fatalf("DeleteVolume (already gone) should succeed: %v", err)
	}
}

func TestBlockControllerPublishVolume_Success(t *testing.T) {
	c, driveMock := newTestBlockController()
	driveMock.drives[100] = &vergeos.VMDrive{ID: 100, Name: "csi-test-pvc", Serial: "csi-abc123", Machine: 10} // on pool VM (machineID=10)

	req := &csi.ControllerPublishVolumeRequest{
		VolumeId: "100",
		NodeId:   "k8s-node-1", // Matches VM in mock (ID=50, Name="k8s-node-1")
		VolumeCapability: &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	resp, err := c.ControllerPublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("ControllerPublishVolume failed: %v", err)
	}
	if resp.PublishContext["serial"] != "csi-abc123" {
		t.Errorf("serial = %q, want %q", resp.PublishContext["serial"], "csi-abc123")
	}
}

func TestBlockControllerUnpublishVolume_Success(t *testing.T) {
	c, driveMock := newTestBlockController()
	driveMock.drives[100] = &vergeos.VMDrive{ID: 100, Name: "csi-test-pvc", Machine: 500} // on node VM (machineID=500)

	req := &csi.ControllerUnpublishVolumeRequest{
		VolumeId: "100",
		NodeId:   "node-1",
	}

	_, err := c.ControllerUnpublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("ControllerUnpublishVolume failed: %v", err)
	}
}

func TestBlockControllerUnpublishVolume_AlreadyDetached(t *testing.T) {
	c, driveMock := newTestBlockController()
	// Drive is on pool VM (machineID=10) — already "detached"
	driveMock.drives[100] = &vergeos.VMDrive{ID: 100, Name: "csi-test-pvc", Machine: 10}

	req := &csi.ControllerUnpublishVolumeRequest{
		VolumeId: "100",
		NodeId:   "node-1",
	}

	_, err := c.ControllerUnpublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("ControllerUnpublishVolume (already detached) should succeed: %v", err)
	}
}

func TestBlockControllerGetCapabilities(t *testing.T) {
	c, _ := newTestBlockController()
	caps := c.ControllerGetCapabilities()

	capTypes := make(map[csi.ControllerServiceCapability_RPC_Type]bool)
	for _, cap := range caps {
		if rpc := cap.GetRpc(); rpc != nil {
			capTypes[rpc.Type] = true
		}
	}

	if !capTypes[csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME] {
		t.Error("expected CREATE_DELETE_VOLUME capability")
	}
	if !capTypes[csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME] {
		t.Error("expected PUBLISH_UNPUBLISH_VOLUME capability")
	}
}

func TestBlockControllerPublishVolume_VMIDLookup(t *testing.T) {
	c, driveMock := newTestBlockController()
	driveMock.drives[100] = &vergeos.VMDrive{ID: 100, Name: "csi-test-pvc", Serial: "csi-abc123", Machine: 10} // on pool VM

	// The mock VM client already has a VM named "k8s-node-1" (ID=50).
	// ControllerPublishVolume should resolve the node name to VM ID 50.

	req := &csi.ControllerPublishVolumeRequest{
		VolumeId: "100",
		NodeId:   "k8s-node-1",
		VolumeCapability: &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
		// Note: NO "vergeos.com/vm-id" in VolumeContext
	}

	resp, err := c.ControllerPublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("ControllerPublishVolume failed: %v", err)
	}
	if resp.PublishContext["serial"] != "csi-abc123" {
		t.Errorf("serial = %q, want %q", resp.PublishContext["serial"], "csi-abc123")
	}
}

func TestBlockControllerPublishVolume_VMNotFound(t *testing.T) {
	c, driveMock := newTestBlockController()
	driveMock.drives[100] = &vergeos.VMDrive{ID: 100, Name: "csi-test-pvc", Serial: "csi-abc123", Machine: 10}

	// mockVMClient has "k8s-node-1" but not "nonexistent-node".

	req := &csi.ControllerPublishVolumeRequest{
		VolumeId: "100",
		NodeId:   "nonexistent-node",
		VolumeCapability: &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	_, err := c.ControllerPublishVolume(context.Background(), req)
	st, _ := status.FromError(err)
	if st.Code() != codes.NotFound {
		t.Errorf("expected NotFound, got %v", err)
	}
}

func TestBlockDeleteVolume_APIError(t *testing.T) {
	c, driveMock := newTestBlockController()
	driveMock.deleteFn = func(_ context.Context, _ int) error {
		return fmt.Errorf("connection refused")
	}

	_, err := c.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "100"})
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("expected Internal, got %v", err)
	}
}

// --- Machine ID and Hotplug tests ---

func TestBlockControllerPublishVolume_MovesDriveToMachineID(t *testing.T) {
	c, driveMock := newTestBlockController()
	driveMock.drives[100] = &vergeos.VMDrive{ID: 100, Name: "csi-test-pvc", Serial: "csi-abc123", Machine: 10}

	var updatedMachineID int
	driveMock.updateFn = func(_ context.Context, _ int, req *vergeos.VMDriveUpdateRequest) (*vergeos.VMDrive, error) {
		if req.Machine != nil {
			updatedMachineID = *req.Machine
		}
		d := driveMock.drives[100]
		d.Machine = *req.Machine
		return d, nil
	}

	req := &csi.ControllerPublishVolumeRequest{
		VolumeId: "100",
		NodeId:   "k8s-node-1", // VM $key=50, machine=500
		VolumeCapability: &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
	}

	_, err := c.ControllerPublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("ControllerPublishVolume failed: %v", err)
	}
	// Must use machine ID (500), not VM $key (50).
	if updatedMachineID != 500 {
		t.Errorf("drive moved to machine %d, want 500 (machine ID, not VM $key 50)", updatedMachineID)
	}
}

func TestBlockControllerPublishVolume_HotplugCalledWithVMKey(t *testing.T) {
	c, driveMock := newTestBlockController()
	driveMock.drives[100] = &vergeos.VMDrive{ID: 100, Name: "csi-test-pvc", Serial: "csi-abc123", Machine: 10}

	var hotplugVMID, hotplugDriveID int
	hotplugMock := &hotplugCapturingMock{
		VMDriveClient:  driveMock,
		hotplugVMID:    &hotplugVMID,
		hotplugDriveID: &hotplugDriveID,
	}
	c.vmDrives = hotplugMock

	req := &csi.ControllerPublishVolumeRequest{
		VolumeId: "100",
		NodeId:   "k8s-node-1",
		VolumeCapability: &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
	}

	_, err := c.ControllerPublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("ControllerPublishVolume failed: %v", err)
	}
	// Hotplug must use VM $key (50), not machine ID (500).
	if hotplugVMID != 50 {
		t.Errorf("hotplug called with VM ID %d, want 50 (VM $key, not machine ID 500)", hotplugVMID)
	}
	if hotplugDriveID != 100 {
		t.Errorf("hotplug called with drive ID %d, want 100", hotplugDriveID)
	}
}

func TestBlockControllerPublishVolume_HotplugError(t *testing.T) {
	c, driveMock := newTestBlockController()
	driveMock.drives[100] = &vergeos.VMDrive{ID: 100, Name: "csi-test-pvc", Serial: "csi-abc123", Machine: 10}

	hotplugMock := &hotplugCapturingMock{
		VMDriveClient: driveMock,
		hotplugErr:    fmt.Errorf("hotplug timeout"),
	}
	c.vmDrives = hotplugMock

	req := &csi.ControllerPublishVolumeRequest{
		VolumeId: "100",
		NodeId:   "k8s-node-1",
		VolumeCapability: &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
	}

	_, err := c.ControllerPublishVolume(context.Background(), req)
	st, _ := status.FromError(err)
	if st.Code() != codes.Unavailable {
		t.Errorf("expected Unavailable error on hotplug failure, got %v", err)
	}
}

func TestBlockControllerPublishVolume_AlreadyOnTarget(t *testing.T) {
	c, driveMock := newTestBlockController()
	// Drive already on target node (machine ID 500).
	driveMock.drives[100] = &vergeos.VMDrive{ID: 100, Name: "csi-test-pvc", Serial: "csi-abc123", Machine: 500}

	req := &csi.ControllerPublishVolumeRequest{
		VolumeId: "100",
		NodeId:   "k8s-node-1",
		VolumeCapability: &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
	}

	resp, err := c.ControllerPublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("expected idempotent success, got: %v", err)
	}
	if resp.PublishContext["serial"] != "csi-abc123" {
		t.Errorf("serial = %q, want %q", resp.PublishContext["serial"], "csi-abc123")
	}
}

func TestBlockControllerUnpublishVolume_HotUnplugOnlineDrive(t *testing.T) {
	c, driveMock := newTestBlockController()
	driveMock.drives[100] = &vergeos.VMDrive{ID: 100, Name: "csi-test-pvc", Machine: 500, PowerState: "online"}

	var unplugVMID, unplugDriveID int
	hotplugMock := &hotplugCapturingMock{
		VMDriveClient:    driveMock,
		hotUnplugVMID:    &unplugVMID,
		hotUnplugDriveID: &unplugDriveID,
	}
	c.vmDrives = hotplugMock

	req := &csi.ControllerUnpublishVolumeRequest{
		VolumeId: "100",
		NodeId:   "k8s-node-1", // VM $key=50
	}

	_, err := c.ControllerUnpublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("ControllerUnpublishVolume failed: %v", err)
	}
	if unplugVMID != 50 {
		t.Errorf("hot-unplug called with VM ID %d, want 50", unplugVMID)
	}
	if unplugDriveID != 100 {
		t.Errorf("hot-unplug called with drive ID %d, want 100", unplugDriveID)
	}
}

func TestBlockControllerUnpublishVolume_SkipsHotUnplugOfflineDrive(t *testing.T) {
	c, driveMock := newTestBlockController()
	// Drive is offline — hot-unplug should be skipped.
	driveMock.drives[100] = &vergeos.VMDrive{ID: 100, Name: "csi-test-pvc", Machine: 500, PowerState: "offline"}

	hotUnplugCalled := false
	hotplugMock := &hotplugCapturingMock{
		VMDriveClient:   driveMock,
		hotUnplugCalled: &hotUnplugCalled,
	}
	c.vmDrives = hotplugMock

	req := &csi.ControllerUnpublishVolumeRequest{
		VolumeId: "100",
		NodeId:   "k8s-node-1",
	}

	_, err := c.ControllerUnpublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("ControllerUnpublishVolume failed: %v", err)
	}
	if hotUnplugCalled {
		t.Error("hot-unplug should not be called for offline drive")
	}
}

func TestBlockControllerUnpublishVolume_MovesToPoolMachineID(t *testing.T) {
	c, driveMock := newTestBlockController()
	driveMock.drives[100] = &vergeos.VMDrive{ID: 100, Name: "csi-test-pvc", Machine: 500}

	var updatedMachineID int
	driveMock.updateFn = func(_ context.Context, _ int, req *vergeos.VMDriveUpdateRequest) (*vergeos.VMDrive, error) {
		if req.Machine != nil {
			updatedMachineID = *req.Machine
		}
		d := driveMock.drives[100]
		d.Machine = *req.Machine
		return d, nil
	}

	req := &csi.ControllerUnpublishVolumeRequest{
		VolumeId: "100",
		NodeId:   "k8s-node-1",
	}

	_, err := c.ControllerUnpublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("ControllerUnpublishVolume failed: %v", err)
	}
	// Must move to pool machine ID (10), not pool VM $key (1).
	if updatedMachineID != 10 {
		t.Errorf("drive moved to machine %d, want 10 (pool machine ID, not VM $key 1)", updatedMachineID)
	}
}

func TestBlockControllerPublishVolume_MissingVolumeID(t *testing.T) {
	c, _ := newTestBlockController()
	_, err := c.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		NodeId: "k8s-node-1",
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", err)
	}
}

func TestBlockControllerPublishVolume_MissingNodeID(t *testing.T) {
	c, _ := newTestBlockController()
	_, err := c.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId: "100",
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", err)
	}
}

func TestBlockControllerPublishVolume_DriveNotFound(t *testing.T) {
	c, _ := newTestBlockController()
	// No drives in mock — drive 100 doesn't exist.
	req := &csi.ControllerPublishVolumeRequest{
		VolumeId: "100",
		NodeId:   "k8s-node-1",
		VolumeCapability: &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
	}

	_, err := c.ControllerPublishVolume(context.Background(), req)
	st, _ := status.FromError(err)
	if st.Code() != codes.NotFound {
		t.Errorf("expected NotFound, got %v", err)
	}
}

func TestBlockControllerUnpublishVolume_DriveNotFound(t *testing.T) {
	c, _ := newTestBlockController()
	// Drive doesn't exist — should succeed (idempotent).
	req := &csi.ControllerUnpublishVolumeRequest{
		VolumeId: "100",
	}

	_, err := c.ControllerUnpublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("expected idempotent success for missing drive, got: %v", err)
	}
}

func TestBlockCreateVolume_CustomInterface(t *testing.T) {
	c, driveMock := newTestBlockController()

	var createdInterface string
	driveMock.createFn = func(_ context.Context, _ int, req *vergeos.VMDriveCreateRequest) (*vergeos.VMDrive, error) {
		createdInterface = req.Interface
		return &vergeos.VMDrive{
			ID:        100,
			Name:      req.Name,
			Serial:    req.Serial,
			SizeBytes: req.SizeBytes,
		}, nil
	}

	req := &csi.CreateVolumeRequest{
		Name:          "test-pvc",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1073741824},
		VolumeCapabilities: []*csi.VolumeCapability{
			{AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}},
		},
		Parameters: map[string]string{"interface": "virtio"},
	}

	_, err := c.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	if createdInterface != "virtio" {
		t.Errorf("interface = %q, want %q", createdInterface, "virtio")
	}
}

func TestBlockCreateVolume_DefaultInterface(t *testing.T) {
	c, driveMock := newTestBlockController()

	var createdInterface string
	driveMock.createFn = func(_ context.Context, _ int, req *vergeos.VMDriveCreateRequest) (*vergeos.VMDrive, error) {
		createdInterface = req.Interface
		return &vergeos.VMDrive{
			ID:        100,
			Name:      req.Name,
			Serial:    req.Serial,
			SizeBytes: req.SizeBytes,
		}, nil
	}

	req := &csi.CreateVolumeRequest{
		Name:          "test-pvc",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1073741824},
		VolumeCapabilities: []*csi.VolumeCapability{
			{AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}},
		},
		Parameters: map[string]string{},
	}

	_, err := c.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	if createdInterface != "virtio-scsi" {
		t.Errorf("interface = %q, want %q (default)", createdInterface, "virtio-scsi")
	}
}

// hotplugCapturingMock wraps a VMDriveClient and captures hotplug/unplug calls.
type hotplugCapturingMock struct {
	VMDriveClient
	hotplugVMID      *int
	hotplugDriveID   *int
	hotplugErr       error
	hotUnplugVMID    *int
	hotUnplugDriveID *int
	hotUnplugErr     error
	hotUnplugCalled  *bool
}

func (m *hotplugCapturingMock) HotplugDrive(_ context.Context, vmID, driveID int) error {
	if m.hotplugVMID != nil {
		*m.hotplugVMID = vmID
	}
	if m.hotplugDriveID != nil {
		*m.hotplugDriveID = driveID
	}
	return m.hotplugErr
}

func (m *hotplugCapturingMock) HotUnplugDrive(_ context.Context, vmID, driveID int) error {
	if m.hotUnplugCalled != nil {
		*m.hotUnplugCalled = true
	}
	if m.hotUnplugVMID != nil {
		*m.hotUnplugVMID = vmID
	}
	if m.hotUnplugDriveID != nil {
		*m.hotUnplugDriveID = driveID
	}
	return m.hotUnplugErr
}
