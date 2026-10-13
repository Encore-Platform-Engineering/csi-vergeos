package block

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	vergeos "github.com/verge-io/govergeos"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	testVMUUID = "0f3c2a58-7a49-4d51-9d0e-6a3b1c2d4e5f"
	// CAPI-style VM name: differs from the Kubernetes Node name.
	testVMName = "default-worker-abc"
)

// nodeVMController returns a controller whose VM list returns `vms` regardless
// of the filter (so tests also prove client-side checks) and records filters.
func nodeVMController(vms ...vergeos.VM) (*BlockController, *[]string) {
	var filters []string
	vmMock := newMockVMClient()
	vmMock.listFn = func(_ context.Context, opts ...vergeos.ListOption) ([]vergeos.VM, error) {
		var o vergeos.ListOptions
		for _, opt := range opts {
			opt(&o)
		}
		filters = append(filters, o.Filter)
		return vms, nil
	}
	return &BlockController{vmDrives: newMockVMDriveClient(), vms: vmMock, poolVMID: 1, poolMachineID: 10}, &filters
}

func TestResolveNodeVM_Forms(t *testing.T) {
	vm := vergeos.VM{ID: 77, UUID: testVMUUID, Name: testVMName, Machine: 770}
	other := vergeos.VM{ID: 78, UUID: "11111111-2222-3333-4444-555555555555", Name: "other", Machine: 780}

	tests := []struct {
		name       string
		nodeID     string
		wantFilter string
	}{
		{"uuid", "uuid:" + testVMUUID, "uuid eq '" + testVMUUID + "' and is_snapshot eq false"},
		{"uuid uppercase", "uuid:" + strings.ToUpper(testVMUUID), "uuid eq '" + testVMUUID + "' and is_snapshot eq false"},
		{"key", "key:77", "$key eq 77 and is_snapshot eq false"},
		{"name prefix", "name:" + testVMName, "name eq '" + testVMName + "' and is_snapshot eq false"},
		{"legacy bare name", testVMName, "name eq '" + testVMName + "' and is_snapshot eq false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, filters := nodeVMController(other, vm)
			got, err := c.resolveNodeVM(context.Background(), tt.nodeID)
			if err != nil {
				t.Fatalf("resolveNodeVM: %v", err)
			}
			if got.vmID != 77 || got.machineID != 770 {
				t.Errorf("got vm=%d machine=%d, want vm=77 machine=770", got.vmID, got.machineID)
			}
			if len(*filters) != 1 || (*filters)[0] != tt.wantFilter {
				t.Errorf("filter = %q, want %q", *filters, tt.wantFilter)
			}
		})
	}
}

func TestResolveNodeVM_NotFound(t *testing.T) {
	c, _ := nodeVMController(vergeos.VM{ID: 78, UUID: "11111111-2222-3333-4444-555555555555", Name: "other"})
	for _, id := range []string{"uuid:" + testVMUUID, "key:77", "name:" + testVMName, testVMName} {
		_, err := c.resolveNodeVM(context.Background(), id)
		if st, _ := status.FromError(err); st.Code() != codes.NotFound {
			t.Errorf("%s: err = %v, want NotFound", id, err)
		}
	}
}

func TestResolveNodeVM_IgnoresSnapshots(t *testing.T) {
	snap := vergeos.VM{ID: 90, UUID: testVMUUID, Name: testVMName, Machine: 900, IsSnapshot: true}
	vm := vergeos.VM{ID: 77, UUID: testVMUUID, Name: testVMName, Machine: 770}

	c, _ := nodeVMController(snap, vm)
	got, err := c.resolveNodeVM(context.Background(), "uuid:"+testVMUUID)
	if err != nil {
		t.Fatalf("snapshot must not cause ambiguity: %v", err)
	}
	if got.vmID != 77 {
		t.Errorf("vmID = %d, want 77 (not the snapshot)", got.vmID)
	}

	c, _ = nodeVMController(snap)
	if _, err := c.resolveNodeVM(context.Background(), "name:"+testVMName); err == nil {
		t.Error("snapshot-only match must not resolve")
	}
}

func TestResolveNodeVM_RejectsAmbiguous(t *testing.T) {
	a := vergeos.VM{ID: 1, UUID: testVMUUID, Name: testVMName, Machine: 10}
	b := vergeos.VM{ID: 2, UUID: testVMUUID, Name: testVMName, Machine: 20}
	for _, id := range []string{"uuid:" + testVMUUID, "name:" + testVMName, testVMName} {
		c, _ := nodeVMController(a, b)
		_, err := c.resolveNodeVM(context.Background(), id)
		if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || !strings.Contains(err.Error(), "2 VergeOS VMs match") {
			t.Errorf("%s: err = %v, want FailedPrecondition ambiguity error", id, err)
		}
	}
}

func TestResolveNodeVM_InvalidIDs(t *testing.T) {
	c, filters := nodeVMController()
	for _, id := range []string{"uuid:not-a-uuid", "key:abc", "key:", "name:", "uuid:" + testVMUUID + "' or name eq 'x"} {
		_, err := c.resolveNodeVM(context.Background(), id)
		if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument {
			t.Errorf("%q: code = %v, want InvalidArgument (err=%v)", id, st.Code(), err)
		}
	}
	if len(*filters) != 0 {
		t.Errorf("invalid IDs must not reach the API, got filters %q", *filters)
	}
}

func TestPublishUnpublish_ByUUIDNodeID(t *testing.T) {
	vm := vergeos.VM{ID: 77, UUID: testVMUUID, Name: testVMName, Machine: 770}
	c, _ := nodeVMController(vm)
	drives := c.vmDrives.(*mockVMDriveClient)
	drives.drives[100] = &vergeos.VMDrive{ID: 100, Name: "csi-pvc", Serial: "csi-abc", Machine: 10}

	ctx := context.Background()
	if _, err := c.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: "100",
		NodeId:   "uuid:" + testVMUUID,
		VolumeCapability: &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := drives.drives[100].Machine; got != 770 {
		t.Errorf("drive machine = %d, want 770", got)
	}

	if _, err := c.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{VolumeId: "100", NodeId: "uuid:" + testVMUUID}); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if got := drives.drives[100].Machine; got != 10 {
		t.Errorf("drive machine = %d, want pool 10", got)
	}
}

func TestResolveNodeVM_EscapesNameFilter(t *testing.T) {
	name := `o'brien\vm`
	c, filters := nodeVMController(vergeos.VM{ID: 5, Name: name, Machine: 50})
	got, err := c.resolveNodeVM(context.Background(), "name:"+name)
	if err != nil || got.vmID != 5 {
		t.Fatalf("got (%v, %v)", got, err)
	}
	want := `name eq 'o\'brien\\vm' and is_snapshot eq false`
	if (*filters)[0] != want {
		t.Errorf("filter = %q, want %q", (*filters)[0], want)
	}
}

func TestResolveNodeVM_APIErrorIsUnavailable(t *testing.T) {
	c, _ := nodeVMController()
	c.vms.(*mockVMClient).listFn = func(context.Context, ...vergeos.ListOption) ([]vergeos.VM, error) {
		return nil, errors.New("connection refused")
	}
	_, err := c.resolveNodeVM(context.Background(), "uuid:"+testVMUUID)
	if st, _ := status.FromError(err); st.Code() != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable (err=%v)", st.Code(), err)
	}
}

// The node ID may differ between publish and unpublish (e.g. name: -> uuid:
// after re-registration) but must still resolve to the VM that owns the drive;
// that VM is the one unplugged.
func TestUnpublish_NodeIDResolvingToOwnerUnplugsIt(t *testing.T) {
	owner := vergeos.VM{ID: 77, UUID: testVMUUID, Name: testVMName, Machine: 770}
	c, _ := nodeVMController(owner)
	drives := c.vmDrives.(*mockVMDriveClient)
	drives.drives[100] = &vergeos.VMDrive{ID: 100, Name: "csi-pvc", Machine: 770, PowerState: "online"}
	var unplugged []int
	c.vmDrives = &unplugRecorder{mockVMDriveClient: drives, vmIDs: &unplugged}

	if _, err := c.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{VolumeId: "100", NodeId: "uuid:" + testVMUUID}); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if len(unplugged) != 1 || unplugged[0] != 77 {
		t.Errorf("unplugged VMs = %v, want [77]", unplugged)
	}
	if drives.drives[100].Machine != 10 {
		t.Errorf("drive machine = %d, want pool 10", drives.drives[100].Machine)
	}
}

// Without a node ID the owner is found via the drive's machine.
func TestUnpublish_NoNodeIDUnplugsOwnerVM(t *testing.T) {
	owner := vergeos.VM{ID: 77, Name: testVMName, Machine: 770}
	c, filters := nodeVMController(owner)
	drives := c.vmDrives.(*mockVMDriveClient)
	drives.drives[100] = &vergeos.VMDrive{ID: 100, Name: "csi-pvc", Machine: 770, PowerState: "online"}
	var unplugged []int
	c.vmDrives = &unplugRecorder{mockVMDriveClient: drives, vmIDs: &unplugged}

	if _, err := c.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{VolumeId: "100"}); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if len(unplugged) != 1 || unplugged[0] != 77 {
		t.Errorf("unplugged VMs = %v, want [77]", unplugged)
	}
	if len(*filters) != 1 || (*filters)[0] != "machine eq 770 and is_snapshot eq false" {
		t.Errorf("filters = %q", *filters)
	}
	if drives.drives[100].Machine != 10 {
		t.Errorf("drive machine = %d, want pool 10", drives.drives[100].Machine)
	}
}

type unplugRecorder struct {
	*mockVMDriveClient
	vmIDs *[]int
}

func (u *unplugRecorder) HotUnplugDrive(_ context.Context, vmID, _ int) error {
	*u.vmIDs = append(*u.vmIDs, vmID)
	return nil
}

func TestUnpublish_NoOwnerVMStillDetaches(t *testing.T) {
	c, _ := nodeVMController() // no VMs at all
	drives := c.vmDrives.(*mockVMDriveClient)
	drives.drives[100] = &vergeos.VMDrive{ID: 100, Name: "csi-pvc", Machine: 770, PowerState: "online"}
	if _, err := c.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{VolumeId: "100"}); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if drives.drives[100].Machine != 10 {
		t.Errorf("drive machine = %d, want pool 10", drives.drives[100].Machine)
	}
}

func TestPublish_AmbiguousNodeIsFailedPrecondition(t *testing.T) {
	c, _ := nodeVMController(
		vergeos.VM{ID: 1, Name: testVMName, Machine: 10},
		vergeos.VM{ID: 2, Name: testVMName, Machine: 20},
	)
	c.vmDrives.(*mockVMDriveClient).drives[100] = &vergeos.VMDrive{ID: 100, Name: "csi-pvc", Machine: 10}
	_, err := c.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId: "100",
		NodeId:   "name:" + testVMName,
		VolumeCapability: &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
	})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition (err=%v)", st.Code(), err)
	}
}
