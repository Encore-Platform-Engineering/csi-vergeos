package driver

import (
	"context"
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
