package driver

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

// NodeService implements the CSI Node server by delegating to a NodeBackend.
type NodeService struct {
	csi.UnimplementedNodeServer
	nodeID   string
	resolver NodeIDResolver // optional; derives the node ID from the Node's providerID
	backend  NodeBackend
}

func NewNodeService(nodeID string, backend NodeBackend) *NodeService {
	return &NodeService{nodeID: nodeID, backend: backend}
}

// WithNodeIDResolver makes NodeGetInfo report the resolver's node ID instead of
// the static one.
func (ns *NodeService) WithNodeIDResolver(r NodeIDResolver) *NodeService {
	ns.resolver = r
	return ns
}

func (ns *NodeService) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	if ns.backend == nil {
		return nil, status.Error(codes.Unimplemented, "node backend not configured")
	}
	return ns.backend.NodeStageVolume(ctx, req)
}

func (ns *NodeService) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	if ns.backend == nil {
		return nil, status.Error(codes.Unimplemented, "node backend not configured")
	}
	return ns.backend.NodeUnstageVolume(ctx, req)
}

func (ns *NodeService) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if ns.backend == nil {
		return nil, status.Error(codes.Unimplemented, "node backend not configured")
	}
	return ns.backend.NodePublishVolume(ctx, req)
}

func (ns *NodeService) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if ns.backend == nil {
		return nil, status.Error(codes.Unimplemented, "node backend not configured")
	}
	return ns.backend.NodeUnpublishVolume(ctx, req)
}

func (ns *NodeService) NodeGetInfo(ctx context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	nodeID := ns.nodeID
	if ns.resolver != nil {
		id, err := ns.resolver.ResolveNodeID(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "resolving node ID: %v", err)
		}
		klog.Infof("NodeGetInfo: node %q -> node ID %q", ns.nodeID, id)
		nodeID = id
	}
	return &csi.NodeGetInfoResponse{NodeId: nodeID}, nil
}

func (ns *NodeService) NodeGetCapabilities(_ context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	if ns.backend == nil {
		return &csi.NodeGetCapabilitiesResponse{}, nil
	}
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: ns.backend.NodeGetCapabilities(),
	}, nil
}
