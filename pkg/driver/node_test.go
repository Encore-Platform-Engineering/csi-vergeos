package driver

import (
	"context"
	"os"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNodeService_NoBackend(t *testing.T) {
	ns := &NodeService{nodeID: "test-node"}

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{})
	if err == nil {
		t.Fatal("expected error when no backend is set")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unimplemented {
		t.Errorf("expected Unimplemented, got %v", err)
	}
}

func TestNodeService_GetInfo(t *testing.T) {
	ns := &NodeService{nodeID: "test-node-123"}

	resp, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo failed: %v", err)
	}
	if resp.NodeId != "test-node-123" {
		t.Errorf("NodeId = %q, want %q", resp.NodeId, "test-node-123")
	}
}

type mockNodeBackend struct {
	publishCalled bool
}

func (m *mockNodeBackend) NodeStageVolume(_ context.Context, _ *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	return &csi.NodeStageVolumeResponse{}, nil
}
func (m *mockNodeBackend) NodeUnstageVolume(_ context.Context, _ *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return &csi.NodeUnstageVolumeResponse{}, nil
}
func (m *mockNodeBackend) NodePublishVolume(_ context.Context, _ *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	m.publishCalled = true
	return &csi.NodePublishVolumeResponse{}, nil
}
func (m *mockNodeBackend) NodeUnpublishVolume(_ context.Context, _ *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	return &csi.NodeUnpublishVolumeResponse{}, nil
}
func (m *mockNodeBackend) NodeGetCapabilities() []*csi.NodeServiceCapability {
	return nil
}

func TestNodeService_DelegatesToBackend(t *testing.T) {
	mock := &mockNodeBackend{}
	ns := &NodeService{nodeID: "test-node", backend: mock}

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		},
	})
	if err != nil {
		t.Fatalf("NodePublishVolume failed: %v", err)
	}
	if !mock.publishCalled {
		t.Error("expected NodePublishVolume to delegate to backend")
	}
}

func TestNodeService_RejectsMissingVolumeCapability(t *testing.T) {
	noAccessType := &csi.VolumeCapability{
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
	for name, volCap := range map[string]*csi.VolumeCapability{
		"nil capability":     nil,
		"no access type set": noAccessType,
	} {
		t.Run(name, func(t *testing.T) {
			mock := &mockNodeBackend{}
			ns := &NodeService{nodeID: "n", backend: mock}

			_, pubErr := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{VolumeCapability: volCap})
			_, stageErr := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{VolumeCapability: volCap})
			for op, err := range map[string]error{"publish": pubErr, "stage": stageErr} {
				if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument {
					t.Errorf("%s: code = %v, want InvalidArgument (err=%v)", op, st.Code(), err)
				}
			}
			if mock.publishCalled {
				t.Error("backend must not be called with an invalid capability")
			}
		})
	}
}

func TestNodeService_AcceptsBlockCapability(t *testing.T) {
	mock := &mockNodeBackend{}
	ns := &NodeService{nodeID: "n", backend: mock}
	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		},
	})
	if err != nil || !mock.publishCalled {
		t.Fatalf("block capability should reach backend: err=%v called=%v", err, mock.publishCalled)
	}
}

func TestNodeService_GetVolumeStats(t *testing.T) {
	ns := &NodeService{nodeID: "n"}
	ctx := context.Background()

	resp, err := ns.NodeGetVolumeStats(ctx, &csi.NodeGetVolumeStatsRequest{VolumeId: "v", VolumePath: t.TempDir()})
	if err != nil {
		t.Fatalf("filesystem path: %v", err)
	}
	if len(resp.Usage) != 2 {
		t.Fatalf("expected BYTES and INODES usage, got %v", resp.Usage)
	}
	for _, u := range resp.Usage {
		if u.Total <= 0 || u.Used < 0 || u.Used > u.Total || u.Available > u.Total {
			t.Errorf("implausible usage %v", u)
		}
	}

	cases := map[string]struct {
		req  *csi.NodeGetVolumeStatsRequest
		code codes.Code
	}{
		"missing path": {&csi.NodeGetVolumeStatsRequest{VolumeId: "v", VolumePath: t.TempDir() + "/nope"}, codes.NotFound},
		"empty id":     {&csi.NodeGetVolumeStatsRequest{VolumePath: t.TempDir()}, codes.InvalidArgument},
		"empty path":   {&csi.NodeGetVolumeStatsRequest{VolumeId: "v"}, codes.InvalidArgument},
	}
	for name, tc := range cases {
		if _, err := ns.NodeGetVolumeStats(ctx, tc.req); status.Code(err) != tc.code {
			t.Errorf("%s: expected %v, got %v", name, tc.code, err)
		}
	}

	// A call still hung on the same path (dead NFS server) is not stacked.
	busy := t.TempDir()
	statsInFlight.Store(busy, struct{}{})
	_, err = ns.NodeGetVolumeStats(ctx, &csi.NodeGetVolumeStatsRequest{VolumeId: "v", VolumePath: busy})
	statsInFlight.Delete(busy)
	if status.Code(err) != codes.Unavailable {
		t.Errorf("in-flight path: expected Unavailable, got %v", err)
	}
	if _, err := ns.NodeGetVolumeStats(ctx, &csi.NodeGetVolumeStatsRequest{VolumeId: "v", VolumePath: busy}); err != nil {
		t.Errorf("path after in-flight call finished: %v", err)
	}
}

func TestNodeService_GetVolumeStatsRawBlock(t *testing.T) {
	// /dev/loop0 exists on most linux hosts; needs root to open it.
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}
	if _, err := os.Stat("/dev/loop0"); err != nil {
		t.Skip("no /dev/loop0")
	}
	ns := &NodeService{nodeID: "n"}
	resp, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumeId: "v", VolumePath: "/dev/loop0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Usage) != 1 || resp.Usage[0].Unit != csi.VolumeUsage_BYTES {
		t.Errorf("expected BYTES total only, got %v", resp.Usage)
	}
}
