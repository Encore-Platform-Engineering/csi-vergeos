package block

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// --- Mocks ---

type mockBlockMounter struct {
	mountedPaths map[string]bool
	formatErr    error
	mountErr     error
	unmountErr   error

	formatCalls []mountCall
	mountCalls  []mountCall
}

type mountCall struct {
	source, target, fsType string
	options                []string
}

func newMockBlockMounter() *mockBlockMounter {
	return &mockBlockMounter{mountedPaths: make(map[string]bool)}
}

func (m *mockBlockMounter) IsMounted(path string) (bool, error) {
	return m.mountedPaths[path], nil
}

func (m *mockBlockMounter) FormatAndMount(source, target, fsType string, options []string) error {
	m.formatCalls = append(m.formatCalls, mountCall{source, target, fsType, options})
	if m.formatErr != nil {
		return m.formatErr
	}
	m.mountedPaths[target] = true
	return nil
}

func (m *mockBlockMounter) Mount(source, target, fsType string, options []string) error {
	m.mountCalls = append(m.mountCalls, mountCall{source, target, fsType, options})
	if m.mountErr != nil {
		return m.mountErr
	}
	m.mountedPaths[target] = true
	return nil
}

func (m *mockBlockMounter) Unmount(target string) error {
	if m.unmountErr != nil {
		return m.unmountErr
	}
	delete(m.mountedPaths, target)
	return nil
}

type mockDeviceDiscovery struct {
	devicePath string
	findErr    error
}

func (m *mockDeviceDiscovery) FindBySerial(serial string) (string, error) {
	if m.findErr != nil {
		return "", m.findErr
	}
	return m.devicePath, nil
}

// --- Tests ---

func newTestBlockNode() (*BlockNode, *mockBlockMounter, *mockDeviceDiscovery) {
	mounter := newMockBlockMounter()
	discovery := &mockDeviceDiscovery{devicePath: "/dev/sdb"}
	node := &BlockNode{
		mounter:   mounter,
		discovery: discovery,
	}
	return node, mounter, discovery
}

// --- NodeStageVolume Tests ---

func TestNodeStageVolume_Success(t *testing.T) {
	node, mounter, _ := newTestBlockNode()
	stagingPath := filepath.Join(t.TempDir(), "staging", "pvc-123")

	req := &csi.NodeStageVolumeRequest{
		VolumeId:          "100",
		StagingTargetPath: stagingPath,
		PublishContext: map[string]string{
			"serial": "csi-abc123",
		},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{
					FsType: "ext4",
				},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	_, err := node.NodeStageVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("NodeStageVolume failed: %v", err)
	}

	if !mounter.mountedPaths[stagingPath] {
		t.Error("expected staging path to be mounted")
	}
}

func TestNodeStageVolume_AlreadyStaged(t *testing.T) {
	node, mounter, _ := newTestBlockNode()
	stagingPath := "/var/lib/kubelet/plugins/csi-block.verge.io/staging/pvc-123"
	mounter.mountedPaths[stagingPath] = true

	req := &csi.NodeStageVolumeRequest{
		VolumeId:          "100",
		StagingTargetPath: stagingPath,
		PublishContext:    map[string]string{"serial": "csi-abc123"},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
	}

	_, err := node.NodeStageVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("NodeStageVolume (idempotent) should succeed: %v", err)
	}
}

func TestNodeStageVolume_DeviceNotFound(t *testing.T) {
	node, _, discovery := newTestBlockNode()
	discovery.findErr = fmt.Errorf("no device found")

	req := &csi.NodeStageVolumeRequest{
		VolumeId:          "100",
		StagingTargetPath: "/staging",
		PublishContext:    map[string]string{"serial": "csi-bad"},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
	}

	_, err := node.NodeStageVolume(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when device not found")
	}
}

func TestNodeStageVolume_MissingSerial(t *testing.T) {
	node, _, _ := newTestBlockNode()

	req := &csi.NodeStageVolumeRequest{
		VolumeId:          "100",
		StagingTargetPath: "/staging",
		PublishContext:    map[string]string{},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
	}

	_, err := node.NodeStageVolume(context.Background(), req)
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", err)
	}
}

// --- NodeUnstageVolume Tests ---

func TestNodeUnstageVolume_Success(t *testing.T) {
	node, mounter, _ := newTestBlockNode()
	stagingPath := "/var/lib/kubelet/plugins/csi-block.verge.io/staging/pvc-123"
	mounter.mountedPaths[stagingPath] = true

	_, err := node.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          "100",
		StagingTargetPath: stagingPath,
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume failed: %v", err)
	}
	if mounter.mountedPaths[stagingPath] {
		t.Error("expected staging path to be unmounted")
	}
}

func TestNodeUnstageVolume_NotStaged(t *testing.T) {
	node, _, _ := newTestBlockNode()

	_, err := node.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          "100",
		StagingTargetPath: "/not/staged",
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume (idempotent) should succeed: %v", err)
	}
}

// --- NodePublishVolume Tests ---

func TestNodePublishVolume_Success(t *testing.T) {
	node, mounter, _ := newTestBlockNode()
	stagingPath := "/staging/pvc-123"
	mounter.mountedPaths[stagingPath] = true
	targetPath := filepath.Join(t.TempDir(), "pods", "uid", "volumes", "csi", "pvc-123", "mount")

	req := &csi.NodePublishVolumeRequest{
		VolumeId:          "100",
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
	}

	_, err := node.NodePublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("NodePublishVolume failed: %v", err)
	}
	if !mounter.mountedPaths[targetPath] {
		t.Error("expected target path to be mounted")
	}
}

func TestNodePublishVolume_AlreadyMounted(t *testing.T) {
	node, mounter, _ := newTestBlockNode()
	targetPath := "/var/lib/kubelet/pods/uid/volumes/csi/pvc-123/mount"
	mounter.mountedPaths[targetPath] = true

	req := &csi.NodePublishVolumeRequest{
		VolumeId:          "100",
		StagingTargetPath: "/staging",
		TargetPath:        targetPath,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
	}

	_, err := node.NodePublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("NodePublishVolume (idempotent) should succeed: %v", err)
	}
}

// --- NodeUnpublishVolume Tests ---

func TestNodeUnpublishVolume_Success(t *testing.T) {
	node, mounter, _ := newTestBlockNode()
	targetPath := "/var/lib/kubelet/pods/uid/mount"
	mounter.mountedPaths[targetPath] = true

	_, err := node.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "100",
		TargetPath: targetPath,
	})
	if err != nil {
		t.Fatalf("NodeUnpublishVolume failed: %v", err)
	}
	if mounter.mountedPaths[targetPath] {
		t.Error("expected target to be unmounted")
	}
}

func TestNodeUnpublishVolume_NotMounted(t *testing.T) {
	node, _, _ := newTestBlockNode()

	_, err := node.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "100",
		TargetPath: "/not/mounted",
	})
	if err != nil {
		t.Fatalf("NodeUnpublishVolume (idempotent) should succeed: %v", err)
	}
}

// --- Capabilities ---

func TestNodeGetCapabilities_Block(t *testing.T) {
	node, _, _ := newTestBlockNode()
	caps := node.NodeGetCapabilities()

	hasStage := false
	for _, cap := range caps {
		if rpc := cap.GetRpc(); rpc != nil {
			if rpc.Type == csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME {
				hasStage = true
			}
		}
	}
	if !hasStage {
		t.Error("block node should advertise STAGE_UNSTAGE_VOLUME")
	}
}

func fsCap(fsType string, flags ...string) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: fsType, MountFlags: flags}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
}

func rawBlockCap() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
}

func TestNodeStageVolume_FsTypeAndMountFlagsReachMounter(t *testing.T) {
	for _, tc := range []struct {
		name, capFs, wantFs string
		flags               []string
	}{
		{"xfs with flags", "xfs", "xfs", []string{"noatime", "nouuid"}},
		{"ext4 with flags", "ext4", "ext4", []string{"discard"}},
		{"empty fsType defaults to ext4", "", "ext4", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node, mounter, _ := newTestBlockNode()
			staging := filepath.Join(t.TempDir(), "staging")
			_, err := node.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
				VolumeId:          "100",
				StagingTargetPath: staging,
				PublishContext:    map[string]string{"serial": "csi-abc123"},
				VolumeCapability:  fsCap(tc.capFs, tc.flags...),
			})
			if err != nil {
				t.Fatalf("NodeStageVolume: %v", err)
			}
			if len(mounter.formatCalls) != 1 {
				t.Fatalf("FormatAndMount calls = %d, want 1", len(mounter.formatCalls))
			}
			got := mounter.formatCalls[0]
			if got.fsType != tc.wantFs || !reflect.DeepEqual(got.options, tc.flags) || got.source != "/dev/sdb" {
				t.Errorf("FormatAndMount(%+v), want fs=%s flags=%v dev=/dev/sdb", got, tc.wantFs, tc.flags)
			}
		})
	}
}

func TestNodeStageVolume_UnsupportedFsTypeRejected(t *testing.T) {
	node, mounter, _ := newTestBlockNode()
	_, err := node.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "100",
		StagingTargetPath: filepath.Join(t.TempDir(), "staging"),
		PublishContext:    map[string]string{"serial": "csi-abc123"},
		VolumeCapability:  fsCap("ntfs"),
	})
	if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err=%v)", st.Code(), err)
	}
	if len(mounter.formatCalls) != 0 {
		t.Error("device must not be formatted for an unsupported fsType")
	}
}

func TestNodeStageVolume_RawBlockNotFormatted(t *testing.T) {
	node, mounter, _ := newTestBlockNode()
	staging := filepath.Join(t.TempDir(), "staging")
	_, err := node.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "100",
		StagingTargetPath: staging,
		PublishContext:    map[string]string{"serial": "csi-abc123"},
		VolumeCapability:  rawBlockCap(),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	if len(mounter.formatCalls) != 0 || len(mounter.mountCalls) != 0 {
		t.Errorf("raw block must not be formatted or mounted at stage: format=%v mount=%v", mounter.formatCalls, mounter.mountCalls)
	}
}

func TestNodePublishVolume_RawBlockBindMountsDevice(t *testing.T) {
	node, mounter, _ := newTestBlockNode()
	target := filepath.Join(t.TempDir(), "publish", "pvc-123")
	_, err := node.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "100",
		TargetPath:        target,
		StagingTargetPath: filepath.Join(t.TempDir(), "staging"),
		PublishContext:    map[string]string{"serial": "csi-abc123"},
		VolumeCapability:  rawBlockCap(),
		Readonly:          true,
	})
	if err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	if len(mounter.mountCalls) != 1 {
		t.Fatalf("Mount calls = %d, want 1", len(mounter.mountCalls))
	}
	got := mounter.mountCalls[0]
	if got.source != "/dev/sdb" || got.target != target || !reflect.DeepEqual(got.options, []string{"bind", "ro"}) {
		t.Errorf("Mount(%+v), want device /dev/sdb bind,ro onto %s", got, target)
	}
	fi, err := os.Stat(target)
	if err != nil || !fi.Mode().IsRegular() {
		t.Errorf("raw block target must be a regular file, got %v (err=%v)", fi, err)
	}
}

func TestNodePublishVolume_StagingNotMountedFailsPrecondition(t *testing.T) {
	node, mounter, _ := newTestBlockNode()
	target := filepath.Join(t.TempDir(), "publish", "pvc-123")
	_, err := node.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "100",
		TargetPath:        target,
		StagingTargetPath: filepath.Join(t.TempDir(), "staging"), // never mounted
		VolumeCapability:  fsCap("ext4"),
	})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", st.Code(), err)
	}
	if len(mounter.mountCalls) != 0 {
		t.Error("must not bind-mount an unstaged path")
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Error("target directory must not be created when staging is missing")
	}
}

func TestNodePublishVolume_FilesystemBindsStaging(t *testing.T) {
	node, mounter, _ := newTestBlockNode()
	target := filepath.Join(t.TempDir(), "publish", "pvc-123")
	staging := filepath.Join(t.TempDir(), "staging")
	mounter.mountedPaths[staging] = true
	_, err := node.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "100",
		TargetPath:        target,
		StagingTargetPath: staging,
		VolumeCapability:  fsCap("ext4"),
	})
	if err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	got := mounter.mountCalls[0]
	if got.source != staging || !reflect.DeepEqual(got.options, []string{"bind"}) {
		t.Errorf("Mount(%+v), want bind of staging dir", got)
	}
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		t.Errorf("filesystem target must be a directory (err=%v)", err)
	}
}
