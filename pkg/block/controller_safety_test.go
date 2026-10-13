package block

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	vergeos "github.com/verge-io/govergeos"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	poolMachine = 10
	nodeAVM     = 50
	nodeAMach   = 500
	nodeBVM     = 51
	nodeBMach   = 510
)

// fakeDrives is a stateful in-memory VMDriveClient. Moving a drive makes it
// offline; hotplug makes it online; unplug makes it offline.
type fakeDrives struct {
	mu     sync.Mutex
	drives map[int]*vergeos.VMDrive
	nextID int

	listErr, updateErr, hotplugErr, unplugErr error

	lists, creates, updates, hotplugs, unplugs int
	hotplugVM, unplugVM                        []int
}

func newFakeDrives() *fakeDrives {
	return &fakeDrives{drives: map[int]*vergeos.VMDrive{}, nextID: 100}
}

func (f *fakeDrives) add(d vergeos.VMDrive) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drives[d.ID.Int()] = &d
}

func (f *fakeDrives) get(id int) vergeos.VMDrive {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.drives[id]
}

func (f *fakeDrives) List(_ context.Context, machine int) ([]vergeos.VMDrive, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []vergeos.VMDrive
	for _, d := range f.drives {
		if d.Machine == machine {
			out = append(out, *d)
		}
	}
	return out, nil
}

func (f *fakeDrives) Get(_ context.Context, id int) (*vergeos.VMDrive, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.drives[id]
	if !ok {
		return nil, &vergeos.NotFoundError{Resource: "VMDrive", ID: id}
	}
	cp := *d
	return &cp, nil
}

func (f *fakeDrives) Create(_ context.Context, machine int, req *vergeos.VMDriveCreateRequest) (*vergeos.VMDrive, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	f.nextID++
	d := &vergeos.VMDrive{ID: vergeos.FlexInt(f.nextID), Name: req.Name, Serial: req.Serial, SizeBytes: req.SizeBytes, Machine: machine}
	f.drives[f.nextID] = d
	cp := *d
	return &cp, nil
}

func (f *fakeDrives) Update(_ context.Context, id int, req *vergeos.VMDriveUpdateRequest) (*vergeos.VMDrive, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates++
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	d, ok := f.drives[id]
	if !ok {
		return nil, &vergeos.NotFoundError{Resource: "VMDrive", ID: id}
	}
	if req.Machine != nil && *req.Machine != d.Machine {
		d.Machine = *req.Machine
		d.PowerState = "offline"
	}
	cp := *d
	return &cp, nil
}

func (f *fakeDrives) Delete(_ context.Context, id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.drives, id)
	return nil
}

func (f *fakeDrives) HotplugDrive(_ context.Context, vmID, id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hotplugs++
	f.hotplugVM = append(f.hotplugVM, vmID)
	if f.hotplugErr != nil {
		return f.hotplugErr
	}
	f.drives[id].PowerState = "online"
	return nil
}

func (f *fakeDrives) HotUnplugDrive(_ context.Context, vmID, id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unplugs++
	f.unplugVM = append(f.unplugVM, vmID)
	if f.unplugErr != nil {
		return f.unplugErr
	}
	f.drives[id].PowerState = "offline"
	return nil
}

type countingVMs struct {
	*mockVMClient
	lists atomic.Int32
}

func (c *countingVMs) List(ctx context.Context, opts ...vergeos.ListOption) ([]vergeos.VM, error) {
	c.lists.Add(1)
	return c.mockVMClient.List(ctx, opts...)
}

// safetyController has two node VMs (node-a, node-b) and a pool at machine 10.
func safetyController() (*BlockController, *fakeDrives, *countingVMs) {
	fd := newFakeDrives()
	vms := &countingVMs{mockVMClient: newMockVMClient()}
	vms.vms[nodeAVM] = &vergeos.VM{ID: nodeAVM, Name: "node-a", Machine: nodeAMach}
	vms.vms[nodeBVM] = &vergeos.VM{ID: nodeBVM, Name: "node-b", Machine: nodeBMach}
	return &BlockController{vmDrives: fd, vms: vms, poolVMID: 1, poolMachineID: poolMachine}, fd, vms
}

func rwo() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
}

func capMode(mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode}}
}

func publish(c *BlockController, volID, node string) error {
	_, err := c.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId: volID, NodeId: node, VolumeCapability: rwo(),
	})
	return err
}

func unpublish(c *BlockController, volID, node string) error {
	_, err := c.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{VolumeId: volID, NodeId: node})
	return err
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("code = %v, want %v (err=%v)", got, want, err)
	}
}

// --- #8: publish safety ---

func TestPublish_AttachedToOtherNodeIsFailedPrecondition(t *testing.T) {
	c, fd, _ := safetyController()
	fd.add(vergeos.VMDrive{ID: 100, Name: "csi-pvc", Machine: nodeAMach, PowerState: "online"})

	wantCode(t, publish(c, "100", "name:node-b"), codes.FailedPrecondition)
	if fd.updates != 0 || fd.hotplugs != 0 || fd.unplugs != 0 {
		t.Errorf("drive touched: updates=%d hotplugs=%d unplugs=%d", fd.updates, fd.hotplugs, fd.unplugs)
	}
	if got := fd.get(100); got.Machine != nodeAMach || got.PowerState != "online" {
		t.Errorf("drive state changed: %+v", got)
	}
}

func TestPublish_RejectsUnsupportedCapabilities(t *testing.T) {
	for name, vc := range map[string]*csi.VolumeCapability{
		"nil":                      nil,
		"RWX":                      capMode(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
		"ROX":                      capMode(csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY),
		"single-writer-multi-node": capMode(csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER),
		"unknown":                  capMode(csi.VolumeCapability_AccessMode_UNKNOWN),
		"no mode":                  {},
		"raw block": {
			AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, fd, _ := safetyController()
			fd.add(vergeos.VMDrive{ID: 100, Name: "csi-pvc", Machine: poolMachine})
			_, err := c.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
				VolumeId: "100", NodeId: "name:node-a", VolumeCapability: vc,
			})
			wantCode(t, err, codes.InvalidArgument)
			if fd.updates != 0 {
				t.Error("drive moved despite invalid capability")
			}
		})
	}
}

func TestPublish_RejectsNonDriverDrive(t *testing.T) {
	c, fd, _ := safetyController()
	fd.add(vergeos.VMDrive{ID: 100, Name: "boot-disk", Machine: poolMachine})
	wantCode(t, publish(c, "100", "name:node-a"), codes.NotFound)
	if fd.updates != 0 || fd.hotplugs != 0 {
		t.Error("non-csi drive was touched")
	}
}

func TestPublish_BadVolumeID(t *testing.T) {
	c, _, _ := safetyController()
	wantCode(t, publish(c, "not-a-number", "name:node-a"), codes.InvalidArgument)
}

func TestUnpublish_NonDriverDriveUntouched(t *testing.T) {
	c, fd, _ := safetyController()
	fd.add(vergeos.VMDrive{ID: 100, Name: "boot-disk", Machine: nodeAMach, PowerState: "online"})
	if err := unpublish(c, "100", "name:node-a"); err != nil {
		t.Fatal(err)
	}
	if fd.updates != 0 || fd.unplugs != 0 {
		t.Error("non-csi drive was touched")
	}
}

// --- #9: publish retry completes the hotplug ---

func TestPublish_RetryAfterHotplugFailureReHotplugs(t *testing.T) {
	c, fd, _ := safetyController()
	fd.add(vergeos.VMDrive{ID: 100, Name: "csi-pvc", Serial: "csi-x", Machine: poolMachine})

	fd.hotplugErr = errors.New("timeout")
	if err := publish(c, "100", "name:node-a"); err == nil {
		t.Fatal("expected error when hotplug fails")
	}
	if got := fd.get(100); got.Machine != nodeAMach || got.PowerState == "online" {
		t.Fatalf("after failed hotplug: %+v", got)
	}

	// Retry while hotplug still fails: must not report success.
	if err := publish(c, "100", "name:node-a"); err == nil {
		t.Fatal("retry reported success although drive is not online")
	}
	if fd.hotplugs != 2 {
		t.Fatalf("hotplug attempts = %d, want 2 (retry must re-invoke hotplug)", fd.hotplugs)
	}

	fd.hotplugErr = nil
	if err := publish(c, "100", "name:node-a"); err != nil {
		t.Fatalf("retry after recovery: %v", err)
	}
	if got := fd.get(100); got.PowerState != "online" {
		t.Errorf("drive not online after successful publish: %+v", got)
	}
	if fd.updates != 1 {
		t.Errorf("updates = %d, want 1 (retry must not move the drive again)", fd.updates)
	}
	if len(fd.hotplugVM) != 3 || fd.hotplugVM[2] != nodeAVM {
		t.Errorf("hotplug VMs = %v, want last call on VM %d", fd.hotplugVM, nodeAVM)
	}

	// Once online, further retries are no-ops.
	if err := publish(c, "100", "name:node-a"); err != nil {
		t.Fatal(err)
	}
	if fd.hotplugs != 3 {
		t.Errorf("hotplugs = %d, want 3 (online drive must not be hotplugged again)", fd.hotplugs)
	}
}

func TestPublish_UpdateFailureIsRetryableAndLeavesDrive(t *testing.T) {
	c, fd, _ := safetyController()
	fd.add(vergeos.VMDrive{ID: 100, Name: "csi-pvc", Machine: poolMachine})
	fd.updateErr = errors.New("api down")
	wantCode(t, publish(c, "100", "name:node-a"), codes.Unavailable)
	if fd.hotplugs != 0 {
		t.Error("hotplug attempted after failed move")
	}
}

// --- #10: unpublish ---

func TestUnpublish_LateCallForOtherNodeDoesNotTouchDrive(t *testing.T) {
	c, fd, _ := safetyController()
	fd.add(vergeos.VMDrive{ID: 100, Name: "csi-pvc", Machine: nodeBMach, PowerState: "online"})

	if err := unpublish(c, "100", "name:node-a"); err != nil {
		t.Fatalf("late unpublish for node-a: %v", err)
	}
	if fd.updates != 0 || fd.unplugs != 0 {
		t.Errorf("drive on node-b touched: updates=%d unplugs=%d", fd.updates, fd.unplugs)
	}
	if got := fd.get(100); got.Machine != nodeBMach || got.PowerState != "online" {
		t.Errorf("drive state changed: %+v", got)
	}
}

func TestUnpublish_UnknownNodeDoesNotTouchDrive(t *testing.T) {
	c, fd, _ := safetyController()
	fd.add(vergeos.VMDrive{ID: 100, Name: "csi-pvc", Machine: nodeBMach, PowerState: "online"})
	if err := unpublish(c, "100", "name:deleted-node"); err != nil {
		t.Fatal(err)
	}
	if fd.updates != 0 || fd.unplugs != 0 {
		t.Error("drive touched for a node that does not exist")
	}
}

func TestUnpublish_HotUnplugFailureDoesNotMove(t *testing.T) {
	c, fd, _ := safetyController()
	fd.add(vergeos.VMDrive{ID: 100, Name: "csi-pvc", Machine: nodeAMach, PowerState: "online"})
	fd.unplugErr = errors.New("guest busy")

	wantCode(t, unpublish(c, "100", "name:node-a"), codes.Unavailable)
	if fd.updates != 0 {
		t.Fatal("drive moved to pool although hot-unplug failed")
	}
	if got := fd.get(100); got.Machine != nodeAMach {
		t.Errorf("drive machine = %d, want unchanged %d", got.Machine, nodeAMach)
	}

	// Retry after recovery completes the detach.
	fd.unplugErr = nil
	if err := unpublish(c, "100", "name:node-a"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := fd.get(100); got.Machine != poolMachine {
		t.Errorf("drive machine = %d, want pool %d", got.Machine, poolMachine)
	}
	if fd.unplugVM[0] != nodeAVM {
		t.Errorf("unplugged VM %d, want %d", fd.unplugVM[0], nodeAVM)
	}
}

func TestUnpublish_AlreadyUnpluggedIsSuccess(t *testing.T) {
	c, fd, _ := safetyController()
	fd.add(vergeos.VMDrive{ID: 100, Name: "csi-pvc", Machine: nodeAMach, PowerState: "online"})
	fd.unplugErr = &vergeos.NotFoundError{Resource: "VMDrive", ID: 100}
	if err := unpublish(c, "100", "name:node-a"); err != nil {
		t.Fatalf("NotFound from unplug must count as unplugged: %v", err)
	}
	if got := fd.get(100); got.Machine != poolMachine {
		t.Errorf("drive machine = %d, want pool", got.Machine)
	}
}

func TestUnpublish_OwnerLookupFailureDoesNotMove(t *testing.T) {
	c, fd, vms := safetyController()
	fd.add(vergeos.VMDrive{ID: 100, Name: "csi-pvc", Machine: nodeAMach, PowerState: "online"})
	vms.listFn = func(context.Context, ...vergeos.ListOption) ([]vergeos.VM, error) {
		return nil, errors.New("api down")
	}
	// No node ID: owner must be discovered from the drive; lookup failure is
	// not a licence to move it.
	wantCode(t, unpublish(c, "100", ""), codes.Unavailable)
	if fd.updates != 0 || fd.unplugs != 0 {
		t.Error("drive touched although owner VM is unknown")
	}
}

func TestUnpublish_UpdateFailureIsRetryable(t *testing.T) {
	c, fd, _ := safetyController()
	fd.add(vergeos.VMDrive{ID: 100, Name: "csi-pvc", Machine: nodeAMach, PowerState: "offline"})
	fd.updateErr = errors.New("api down")
	wantCode(t, unpublish(c, "100", "name:node-a"), codes.Unavailable)
}

func TestUnpublish_GetErrorIsUnavailable(t *testing.T) {
	c, fd, _ := safetyController()
	c.vmDrives = &getFailing{fakeDrives: fd}
	wantCode(t, unpublish(c, "100", "name:node-a"), codes.Unavailable)
}

type getFailing struct{ *fakeDrives }

func (g *getFailing) Get(context.Context, int) (*vergeos.VMDrive, error) {
	return nil, errors.New("api down")
}

// --- #11: CreateVolume ---

func createReq(name string, required, limit int64, caps ...*csi.VolumeCapability) *csi.CreateVolumeRequest {
	if len(caps) == 0 {
		caps = []*csi.VolumeCapability{rwo()}
	}
	req := &csi.CreateVolumeRequest{Name: name, VolumeCapabilities: caps}
	if required != 0 || limit != 0 {
		req.CapacityRange = &csi.CapacityRange{RequiredBytes: required, LimitBytes: limit}
	}
	return req
}

func TestCreate_RejectsUnsupportedCapabilities(t *testing.T) {
	for name, caps := range map[string][]*csi.VolumeCapability{
		"RWX":     {capMode(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)},
		"ROX":     {capMode(csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY)},
		"mixed":   {rwo(), capMode(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)},
		"nil cap": {nil},
		"no caps": nil,
		"no mode": {{}},
		"raw block": {{
			AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			c, fd, _ := safetyController()
			_, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{Name: "pvc", VolumeCapabilities: caps})
			wantCode(t, err, codes.InvalidArgument)
			if fd.creates != 0 {
				t.Error("drive created despite invalid capability")
			}
		})
	}
}

func TestCreate_AcceptsSingleNodeModes(t *testing.T) {
	for _, m := range []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
	} {
		c, _, _ := safetyController()
		if _, err := c.CreateVolume(context.Background(), createReq("pvc", 0, 0, capMode(m))); err != nil {
			t.Errorf("%s rejected: %v", m, err)
		}
	}
}

func TestCreate_ListErrorDoesNotCreate(t *testing.T) {
	c, fd, _ := safetyController()
	fd.listErr = errors.New("transient")
	_, err := c.CreateVolume(context.Background(), createReq("pvc", 1<<30, 0))
	wantCode(t, err, codes.Unavailable)
	if fd.creates != 0 {
		t.Fatal("Create called although existing drives could not be listed")
	}

	// Even a NotFound-flavoured list error must not be read as "no drives".
	fd.listErr = &vergeos.NotFoundError{Resource: "VMDrive", ID: poolMachine}
	_, err = c.CreateVolume(context.Background(), createReq("pvc", 1<<30, 0))
	wantCode(t, err, codes.Unavailable)
	if fd.creates != 0 {
		t.Fatal("Create called after NotFound list error")
	}
}

func TestCreate_SizeMismatchIsAlreadyExists(t *testing.T) {
	c, fd, _ := safetyController()
	fd.add(vergeos.VMDrive{ID: 100, Name: "csi-pvc", SizeBytes: 1 << 30, Machine: poolMachine})

	_, err := c.CreateVolume(context.Background(), createReq("pvc", 2<<30, 0))
	wantCode(t, err, codes.AlreadyExists)

	// Existing drive exceeds the new limit.
	_, err = c.CreateVolume(context.Background(), createReq("pvc", 0, 512<<20))
	wantCode(t, err, codes.AlreadyExists)

	if fd.creates != 0 {
		t.Error("duplicate drive created")
	}
}

func TestCreate_CompatibleExistingIsReused(t *testing.T) {
	c, fd, _ := safetyController()
	fd.add(vergeos.VMDrive{ID: 100, Name: "csi-pvc", Serial: "s", SizeBytes: 2 << 30, Machine: poolMachine})
	for _, r := range []*csi.CreateVolumeRequest{
		createReq("pvc", 2<<30, 0),     // exact
		createReq("pvc", 1<<30, 4<<30), // within [required, limit]
		createReq("pvc", 0, 0),         // no range: whatever exists
	} {
		resp, err := c.CreateVolume(context.Background(), r)
		if err != nil {
			t.Fatalf("%v: %v", r.CapacityRange, err)
		}
		if resp.Volume.VolumeId != "100" || resp.Volume.CapacityBytes != 2<<30 {
			t.Errorf("resp = %v", resp.Volume)
		}
	}
	if fd.creates != 0 {
		t.Error("drive created although a compatible one exists")
	}
}

func TestCreate_RequiredAboveLimitIsOutOfRange(t *testing.T) {
	c, fd, _ := safetyController()
	_, err := c.CreateVolume(context.Background(), createReq("pvc", 2<<30, 1<<30))
	wantCode(t, err, codes.OutOfRange)
	if fd.creates != 0 {
		t.Error("drive created")
	}
}

func TestCreate_NegativeCapacityIsInvalid(t *testing.T) {
	c, _, _ := safetyController()
	_, err := c.CreateVolume(context.Background(), createReq("pvc", -1, 0))
	wantCode(t, err, codes.InvalidArgument)
}

func TestCreate_SizeHonorsLimit(t *testing.T) {
	c, fd, _ := safetyController()
	// No required bytes: default 1 GiB would exceed the 512 MiB limit.
	resp, err := c.CreateVolume(context.Background(), createReq("small", 0, 512<<20))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Volume.CapacityBytes != 512<<20 {
		t.Errorf("capacity = %d, want limit 512MiB", resp.Volume.CapacityBytes)
	}
	// Default when unconstrained.
	resp, err = c.CreateVolume(context.Background(), createReq("dflt", 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Volume.CapacityBytes != 1<<30 {
		t.Errorf("default capacity = %d, want 1GiB", resp.Volume.CapacityBytes)
	}
	if fd.creates != 2 {
		t.Errorf("creates = %d", fd.creates)
	}
}

func TestCreate_DoesNotScanOtherVMs(t *testing.T) {
	c, fd, vms := safetyController()
	// Same name exists elsewhere (another cluster / a node VM): must be ignored.
	fd.add(vergeos.VMDrive{ID: 7, Name: "csi-pvc", SizeBytes: 1 << 30, Machine: nodeAMach})
	resp, err := c.CreateVolume(context.Background(), createReq("pvc", 1<<30, 0))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Volume.VolumeId == "7" {
		t.Error("adopted a drive that is not on the pool VM")
	}
	if n := vms.lists.Load(); n != 0 {
		t.Errorf("VM list called %d times, want 0", n)
	}
	if fd.lists != 1 {
		t.Errorf("drive list calls = %d, want 1 (pool VM only)", fd.lists)
	}
}

func TestCreate_DuplicateNamesOnPoolAreRejected(t *testing.T) {
	c, fd, _ := safetyController()
	fd.add(vergeos.VMDrive{ID: 1, Name: "csi-pvc", SizeBytes: 1 << 30, Machine: poolMachine})
	fd.add(vergeos.VMDrive{ID: 2, Name: "csi-pvc", SizeBytes: 1 << 30, Machine: poolMachine})
	_, err := c.CreateVolume(context.Background(), createReq("pvc", 1<<30, 0))
	wantCode(t, err, codes.FailedPrecondition)
	if fd.creates != 0 {
		t.Error("created a third drive")
	}
}

func TestCreate_ConcurrentSameNameCreatesOnce(t *testing.T) {
	c, fd, _ := safetyController()
	const n = 16
	var wg sync.WaitGroup
	ids := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := c.CreateVolume(context.Background(), createReq("pvc", 1<<30, 0))
			errs[i] = err
			if err == nil {
				ids[i] = resp.Volume.VolumeId
			}
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Errorf("call %d returned %s, call 0 returned %s", i, ids[i], ids[0])
		}
	}
	if fd.creates != 1 {
		t.Errorf("creates = %d, want 1", fd.creates)
	}
}

func TestKeyedLock_SerializesPerKeyAndCleansUp(t *testing.T) {
	var k keyedLock
	var inside atomic.Int32
	var maxSeen atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock, err := k.lock(context.Background(), "a")
			if err != nil {
				t.Error(err)
				return
			}
			n := inside.Add(1)
			if n > maxSeen.Load() {
				maxSeen.Store(n)
			}
			inside.Add(-1)
			unlock()
		}()
	}
	wg.Wait()
	if maxSeen.Load() != 1 {
		t.Errorf("max concurrent holders = %d, want 1", maxSeen.Load())
	}
	// Different keys do not block each other.
	u1, _ := k.lock(context.Background(), "x")
	u2, _ := k.lock(context.Background(), "y")
	u1()
	u2()
	if len(k.entries) != 0 {
		t.Errorf("%d lock entries leaked", len(k.entries))
	}
}

// --- ValidateVolumeCapabilities ---

func TestValidateVolumeCapabilities_NilAccessModeAndBlock(t *testing.T) {
	c, _, _ := safetyController()
	for name, vc := range map[string]*csi.VolumeCapability{
		"no access mode": {},
		"raw block": {
			AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
		"RWX": capMode(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
	} {
		resp, err := c.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
			VolumeId: "1", VolumeCapabilities: []*csi.VolumeCapability{vc},
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if resp.Confirmed != nil || resp.Message == "" {
			t.Errorf("%s: confirmed=%v message=%q, want rejection with message", name, resp.Confirmed, resp.Message)
		}
	}
	resp, err := c.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: "1", VolumeCapabilities: []*csi.VolumeCapability{rwo()},
	})
	if err != nil || resp.Confirmed == nil {
		t.Errorf("RWO not confirmed: %v %v", resp, err)
	}
}

// --- review follow-ups ---

func TestUnpublish_DriveStrandedOnDeletedVMHealsToPool(t *testing.T) {
	c, fd, _ := safetyController()
	// Machine 999 belongs to no VM any more (node deleted).
	fd.add(vergeos.VMDrive{ID: 100, Name: "csi-pvc", Machine: 999, PowerState: "online"})

	if err := unpublish(c, "100", "name:node-gone"); err != nil {
		t.Fatal(err)
	}
	if fd.unplugs != 0 {
		t.Error("hot-unplug attempted on a VM that no longer exists")
	}
	if got := fd.get(100); got.Machine != poolMachine {
		t.Fatalf("drive machine = %d, want pool %d", got.Machine, poolMachine)
	}
	// Self-healed: the drive can now be published elsewhere.
	if err := publish(c, "100", "name:node-b"); err != nil {
		t.Fatalf("publish after heal: %v", err)
	}
	if got := fd.get(100); got.Machine != nodeBMach || got.PowerState != "online" {
		t.Errorf("drive = %+v", got)
	}
}

func TestUnpublish_OwnerLookupErrorWithGoneNodeDoesNotMove(t *testing.T) {
	c, fd, vms := safetyController()
	fd.add(vergeos.VMDrive{ID: 100, Name: "csi-pvc", Machine: 999})
	vms.listFn = func(_ context.Context, opts ...vergeos.ListOption) ([]vergeos.VM, error) {
		var o vergeos.ListOptions
		for _, opt := range opts {
			opt(&o)
		}
		if o.Filter == "machine eq 999 and is_snapshot eq false" {
			return nil, errors.New("api down")
		}
		return nil, nil // node lookup: not found
	}
	wantCode(t, unpublish(c, "100", "name:node-gone"), codes.Unavailable)
	if fd.updates != 0 {
		t.Error("drive moved although its holder could not be determined")
	}
}

func TestDelete_RefusesNonDriverDrive(t *testing.T) {
	c, fd, _ := safetyController()
	fd.add(vergeos.VMDrive{ID: 100, Name: "boot-disk", Machine: poolMachine})
	if _, err := c.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "100"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := fd.drives[100]; !ok {
		t.Fatal("non-csi drive was deleted")
	}
}

func TestDelete_RefusesAttachedDrive(t *testing.T) {
	c, fd, _ := safetyController()
	fd.add(vergeos.VMDrive{ID: 100, Name: "csi-pvc", Machine: nodeAMach, PowerState: "online"})
	_, err := c.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "100"})
	wantCode(t, err, codes.FailedPrecondition)
	if _, ok := fd.drives[100]; !ok {
		t.Fatal("attached drive was deleted")
	}
}

func TestDelete_PoolDriveAndMissingDrive(t *testing.T) {
	c, fd, _ := safetyController()
	fd.add(vergeos.VMDrive{ID: 100, Name: "csi-pvc", Machine: poolMachine})
	for i := 0; i < 2; i++ { // second call: already gone, still OK
		if _, err := c.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "100"}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, ok := fd.drives[100]; ok {
		t.Fatal("pool drive not deleted")
	}
}

func TestCreate_NoCapacityRangeAcceptsSmallerExisting(t *testing.T) {
	c, fd, _ := safetyController()
	fd.add(vergeos.VMDrive{ID: 100, Name: "csi-pvc", SizeBytes: 512 << 20, Machine: poolMachine})
	resp, err := c.CreateVolume(context.Background(), createReq("pvc", 0, 0))
	if err != nil {
		t.Fatalf("no range must mean no minimum: %v", err)
	}
	if resp.Volume.VolumeId != "100" || fd.creates != 0 {
		t.Errorf("resp=%v creates=%d", resp.Volume, fd.creates)
	}
}

func TestKeyedLock_WaiterHonoursContext(t *testing.T) {
	var k keyedLock
	unlock, err := k.lock(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := k.lock(ctx, "a"); status.Code(err) != codes.Aborted {
		t.Fatalf("err = %v, want Aborted", err)
	}
	unlock()
	if len(k.entries) != 0 {
		t.Errorf("%d lock entries leaked", len(k.entries))
	}
	// Lock is usable again.
	u, err := k.lock(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	u()
}

func TestCreate_CreateErrorIsUnavailable(t *testing.T) {
	c, fd, _ := safetyController()
	c.vmDrives = &createFailing{fakeDrives: fd}
	_, err := c.CreateVolume(context.Background(), createReq("pvc", 1<<30, 0))
	wantCode(t, err, codes.Unavailable)
}

type createFailing struct{ *fakeDrives }

func (c *createFailing) Create(context.Context, int, *vergeos.VMDriveCreateRequest) (*vergeos.VMDrive, error) {
	return nil, errors.New("api down")
}
