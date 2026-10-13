package nas

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// --- Mock Mounter ---

type mockMounter struct {
	mountedPaths map[string]bool
	mountErr     error
	unmountErr   error

	mountCalls  int
	lastSource  string
	lastOptions []string
}

func newMockMounter() *mockMounter {
	return &mockMounter{mountedPaths: make(map[string]bool)}
}

func (m *mockMounter) IsMounted(path string) (bool, error) {
	return m.mountedPaths[path], nil
}

func (m *mockMounter) MountNFS(source, target string, options []string) error {
	m.mountCalls++
	m.lastSource = source
	m.lastOptions = options
	if m.mountErr != nil {
		return m.mountErr
	}
	m.mountedPaths[target] = true
	return nil
}

func (m *mockMounter) Unmount(target string) error {
	if m.unmountErr != nil {
		return m.unmountErr
	}
	delete(m.mountedPaths, target)
	return nil
}

// --- Tests ---

func newTestNode() (*NASNode, *mockMounter) {
	mounter := newMockMounter()
	node := &NASNode{
		mounter: mounter,
	}
	return node, mounter
}

func TestNodePublishVolume_Success(t *testing.T) {
	node, mounter := newTestNode()

	req := &csi.NodePublishVolumeRequest{
		VolumeId:   "1:vol123:share456",
		TargetPath: "/var/lib/kubelet/pods/uid/volumes/csi/pvc-123/mount",
		VolumeContext: map[string]string{
			"nasServiceIP": "10.0.0.5",
			"volumeName":   "csi-test-pvc",
		},
		VolumeCapability: &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			},
		},
	}

	_, err := node.NodePublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("NodePublishVolume failed: %v", err)
	}

	if !mounter.mountedPaths[req.TargetPath] {
		t.Error("expected target path to be mounted")
	}
}

func TestNodePublishVolume_AlreadyMounted(t *testing.T) {
	node, mounter := newTestNode()
	targetPath := "/var/lib/kubelet/pods/uid/volumes/csi/pvc-123/mount"
	mounter.mountedPaths[targetPath] = true

	req := &csi.NodePublishVolumeRequest{
		VolumeId:   "1:vol123:share456",
		TargetPath: targetPath,
		VolumeContext: map[string]string{
			"nasServiceIP": "10.0.0.5",
			"volumeName":   "csi-test-pvc",
		},
		VolumeCapability: &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			},
		},
	}

	_, err := node.NodePublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("NodePublishVolume (idempotent) should succeed: %v", err)
	}
}

func TestNodePublishVolume_MissingTargetPath(t *testing.T) {
	node, _ := newTestNode()

	req := &csi.NodePublishVolumeRequest{
		VolumeId:   "1:vol123:share456",
		TargetPath: "",
	}
	_, err := node.NodePublishVolume(context.Background(), req)
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", err)
	}
}

func TestNodePublishVolume_MountFailure(t *testing.T) {
	node, mounter := newTestNode()
	mounter.mountErr = fmt.Errorf("mount failed")

	req := &csi.NodePublishVolumeRequest{
		VolumeId:   "1:vol123:share456",
		TargetPath: "/target",
		VolumeContext: map[string]string{
			"nasServiceIP": "10.0.0.5",
			"volumeName":   "csi-test-pvc",
		},
		VolumeCapability: &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			},
		},
	}
	_, err := node.NodePublishVolume(context.Background(), req)
	if err == nil {
		t.Fatal("expected error on mount failure")
	}
}

func TestNodeUnpublishVolume_Success(t *testing.T) {
	node, mounter := newTestNode()
	targetPath := "/var/lib/kubelet/pods/uid/volumes/csi/pvc-123/mount"
	mounter.mountedPaths[targetPath] = true

	req := &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "1:vol123:share456",
		TargetPath: targetPath,
	}

	_, err := node.NodeUnpublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("NodeUnpublishVolume failed: %v", err)
	}

	if mounter.mountedPaths[targetPath] {
		t.Error("expected target path to be unmounted")
	}
}

func TestNodeUnpublishVolume_NotMounted(t *testing.T) {
	node, _ := newTestNode()

	req := &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "1:vol123:share456",
		TargetPath: "/not/mounted",
	}

	_, err := node.NodeUnpublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("NodeUnpublishVolume (idempotent) should succeed: %v", err)
	}
}

func TestNodeGetCapabilities_NAS(t *testing.T) {
	node, _ := newTestNode()
	caps := node.NodeGetCapabilities()

	// NAS should NOT advertise STAGE_UNSTAGE_VOLUME.
	for _, cap := range caps {
		if rpc := cap.GetRpc(); rpc != nil {
			if rpc.Type == csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME {
				t.Error("NAS node should not advertise STAGE_UNSTAGE_VOLUME")
			}
		}
	}
}

func nasPublishReq(volCap *csi.VolumeCapability, readonly bool) *csi.NodePublishVolumeRequest {
	return &csi.NodePublishVolumeRequest{
		VolumeId:   "1:vol123:share456",
		TargetPath: "/target",
		Readonly:   readonly,
		VolumeContext: map[string]string{
			"nasServiceIP": "10.0.0.5",
			"volumeName":   "csi-test-pvc",
		},
		VolumeCapability: volCap,
	}
}

func mountCap(flags ...string) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{MountFlags: flags}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
	}
}

func TestNodePublishVolume_MountOptionsReachMounter(t *testing.T) {
	tests := []struct {
		name     string
		flags    []string
		readonly bool
		want     []string
	}{
		{"defaults when no flags", nil, false, []string{"hard", "nolock"}},
		{"user flags are passed through", []string{"rsize=65536"}, false, []string{"hard", "rsize=65536", "nolock"}},
		{"nfsvers=4.1 passed and nolock dropped", []string{"nfsvers=4.1"}, false, []string{"hard", "nfsvers=4.1"}},
		{"vers=4 also drops nolock", []string{"vers=4"}, false, []string{"hard", "vers=4"}},
		{"nfsvers=3 keeps nolock", []string{"nfsvers=3"}, false, []string{"hard", "nfsvers=3", "nolock"}},
		{"user lock respected", []string{"lock"}, false, []string{"hard", "lock"}},
		{"user soft replaces hard", []string{"soft"}, false, []string{"soft", "nolock"}},
		{"readonly appended", nil, true, []string{"hard", "nolock", "ro"}},
		{"readonly not duplicated", []string{"ro"}, true, []string{"hard", "ro", "nolock"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node, mounter := newTestNode()
			if _, err := node.NodePublishVolume(context.Background(), nasPublishReq(mountCap(tc.flags...), tc.readonly)); err != nil {
				t.Fatalf("NodePublishVolume: %v", err)
			}
			if !reflect.DeepEqual(mounter.lastOptions, tc.want) {
				t.Errorf("options = %v, want %v", mounter.lastOptions, tc.want)
			}
			if mounter.lastSource != "10.0.0.5:/mnt/csi-test-pvc" {
				t.Errorf("source = %q", mounter.lastSource)
			}
		})
	}
}

func TestNodePublishVolume_BlockCapabilityRejected(t *testing.T) {
	node, mounter := newTestNode()
	blockCap := &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
	}
	_, err := node.NodePublishVolume(context.Background(), nasPublishReq(blockCap, false))
	if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err=%v)", st.Code(), err)
	}
	if mounter.mountCalls != 0 {
		t.Error("NFS must not be mounted for a block-mode capability")
	}
}
