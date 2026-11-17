package driver

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// NodeGetVolumeStats reports usage for a published volume. It is backend-agnostic:
// filesystem volumes are statfs'd, raw block volumes report the device size.
func (ns *NodeService) NodeGetVolumeStats(_ context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	path := req.GetVolumePath()
	if path == "" {
		return nil, status.Error(codes.InvalidArgument, "volume path is required")
	}
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, status.Errorf(codes.NotFound, "volume path %s does not exist", path)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "stat %s: %v", path, err)
	}

	if fi.Mode()&fs.ModeDevice != 0 {
		size, err := deviceSize(path)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "reading device size of %s: %v", path, err)
		}
		return &csi.NodeGetVolumeStatsResponse{Usage: []*csi.VolumeUsage{
			{Unit: csi.VolumeUsage_BYTES, Total: size},
		}}, nil
	}

	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return nil, status.Errorf(codes.Internal, "statfs %s: %v", path, err)
	}
	bsize := int64(st.Bsize)
	total := int64(st.Blocks) * bsize
	avail := int64(st.Bavail) * bsize
	return &csi.NodeGetVolumeStatsResponse{Usage: []*csi.VolumeUsage{
		{Unit: csi.VolumeUsage_BYTES, Total: total, Available: avail, Used: total - int64(st.Bfree)*bsize},
		{Unit: csi.VolumeUsage_INODES, Total: int64(st.Files), Available: int64(st.Ffree), Used: int64(st.Files) - int64(st.Ffree)},
	}}, nil
}

// deviceSize returns a block device's size by seeking to its end (same result as BLKGETSIZE64).
func deviceSize(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.Seek(0, io.SeekEnd)
}
