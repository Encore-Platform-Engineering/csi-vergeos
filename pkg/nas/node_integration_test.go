//go:build integration

package nas

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/verge-io/csi-vergeos/pkg/util"
)

// newIntegrationNode creates a NASNode using the real OS mounter.
// This requires root privileges for NFS mount/unmount operations.
func newIntegrationNode(t *testing.T) *NASNode {
	t.Helper()

	// NFS mount requires root on Linux; macOS may also need elevated privileges.
	if os.Getuid() != 0 {
		t.Skip("skipping NFS mount integration test: requires root privileges (run with sudo)")
	}

	// Verify NFS client is available.
	if _, err := exec.LookPath("mount.nfs"); err != nil {
		// On macOS, the mount command handles NFS directly.
		if _, err := exec.LookPath("mount_nfs"); err != nil {
			t.Skip("skipping NFS mount integration test: no NFS client found (install nfs-common or nfs-utils)")
		}
	}

	return NewNASNode()
}

// createTestVolumeForNode creates a NAS volume and returns the volume ID and
// volume context needed for NodePublishVolume. The volume is automatically
// cleaned up when the test completes.
func createTestVolumeForNode(t *testing.T) (volumeID string, volumeContext map[string]string) {
	t.Helper()

	c := newIntegrationController(t)
	ctx := context.Background()

	volumeName := fmt.Sprintf("csi-test-node-%d", time.Now().UnixNano())

	t.Cleanup(func() {
		if volumeID != "" {
			_, err := c.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: volumeID})
			if err != nil {
				t.Logf("cleanup: DeleteVolume(%s) failed: %v", volumeID, err)
			} else {
				t.Logf("cleanup: deleted volume %s", volumeID)
			}
		}
	})

	createReq := &csi.CreateVolumeRequest{
		Name: volumeName,
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
			"nasServiceName": testNASServiceName,
			"allowedHosts":   "10.0.0.0/8",
			"nasServiceIP":   testNASServiceIP,
		},
	}

	resp, err := c.CreateVolume(ctx, createReq)
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	if resp.Volume == nil {
		t.Fatal("response volume is nil")
	}

	volumeID = resp.Volume.VolumeId
	volumeContext = resp.Volume.VolumeContext
	t.Logf("Created test volume: %s (context: %v)", volumeID, volumeContext)

	return volumeID, volumeContext
}

// verifyNASServiceReachable checks that the NAS service IP is reachable.
// This catches the case where we have credentials but the NAS VM isn't
// accessible from this machine.
func verifyNASServiceReachable(t *testing.T) {
	t.Helper()

	// Try to verify the NAS service is reachable. The NAS service IP
	// from testNASServiceIP may not be set to the real IP. If
	// VERGEOS_NAS_IP is set, use that instead.
	nasIP := os.Getenv("VERGEOS_NAS_IP")
	if nasIP == "" {
		nasIP = testNASServiceIP
	}

	// Quick check: try connecting to NFS port (2049).
	// We use a short timeout to fail fast if unreachable.
	cmd := exec.Command("nc", "-z", "-w", "3", nasIP, "2049")
	if err := cmd.Run(); err != nil {
		t.Skipf("skipping NFS mount integration test: NAS service at %s:2049 is unreachable", nasIP)
	}
}

func TestIntegration_NodePublishAndUnpublish(t *testing.T) {
	node := newIntegrationNode(t)
	verifyNASServiceReachable(t)

	volumeID, volumeCtx := createTestVolumeForNode(t)
	ctx := context.Background()

	// Use a temp dir as the mount target (simulating kubelet's pod volume dir).
	targetPath := filepath.Join(t.TempDir(), "mount")

	// Ensure cleanup: unmount on failure.
	t.Cleanup(func() {
		mounter := util.NewMounter()
		mounted, err := mounter.IsMounted(targetPath)
		if err == nil && mounted {
			t.Logf("cleanup: unmounting %s", targetPath)
			if err := mounter.Unmount(targetPath); err != nil {
				t.Logf("cleanup: unmount %s failed: %v", targetPath, err)
			}
		}
	})

	// Override the nasServiceIP in the volume context if VERGEOS_NAS_IP is set.
	if nasIP := os.Getenv("VERGEOS_NAS_IP"); nasIP != "" {
		volumeCtx["nasServiceIP"] = nasIP
		t.Logf("Using VERGEOS_NAS_IP override: %s", nasIP)
	}

	// --- NodePublishVolume (mount) ---
	publishReq := &csi.NodePublishVolumeRequest{
		VolumeId:      volumeID,
		TargetPath:    targetPath,
		VolumeContext: volumeCtx,
		VolumeCapability: &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			},
		},
	}

	_, err := node.NodePublishVolume(ctx, publishReq)
	if err != nil {
		t.Fatalf("NodePublishVolume failed: %v", err)
	}
	t.Logf("NodePublishVolume succeeded: mounted at %s", targetPath)

	// Verify the mount is actually there.
	mounter := util.NewMounter()
	mounted, err := mounter.IsMounted(targetPath)
	if err != nil {
		t.Fatalf("checking mount status: %v", err)
	}
	if !mounted {
		t.Fatal("target path is not mounted after NodePublishVolume")
	}

	// Verify we can write to the mount (NFS share should be writable).
	testFile := filepath.Join(targetPath, "csi-integration-test.txt")
	if err := os.WriteFile(testFile, []byte("csi integration test\n"), 0644); err != nil {
		t.Logf("warning: could not write test file (NFS share may be read-only): %v", err)
	} else {
		t.Log("Successfully wrote test file to NFS mount")
		// Clean up test file.
		os.Remove(testFile)
	}

	// --- NodeUnpublishVolume (unmount) ---
	unpublishReq := &csi.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
	}

	_, err = node.NodeUnpublishVolume(ctx, unpublishReq)
	if err != nil {
		t.Fatalf("NodeUnpublishVolume failed: %v", err)
	}
	t.Logf("NodeUnpublishVolume succeeded: unmounted %s", targetPath)

	// Verify no longer mounted.
	mounted, err = mounter.IsMounted(targetPath)
	if err != nil {
		t.Logf("checking mount status after unmount: %v (may be expected if dir removed)", err)
	} else if mounted {
		t.Error("target path is still mounted after NodeUnpublishVolume")
	}
}

func TestIntegration_NodePublishVolume_Idempotent(t *testing.T) {
	node := newIntegrationNode(t)
	verifyNASServiceReachable(t)

	volumeID, volumeCtx := createTestVolumeForNode(t)
	ctx := context.Background()

	targetPath := filepath.Join(t.TempDir(), "mount")

	// Ensure cleanup: unmount on failure.
	t.Cleanup(func() {
		mounter := util.NewMounter()
		mounted, err := mounter.IsMounted(targetPath)
		if err == nil && mounted {
			t.Logf("cleanup: unmounting %s", targetPath)
			if err := mounter.Unmount(targetPath); err != nil {
				t.Logf("cleanup: unmount %s failed: %v", targetPath, err)
			}
		}
	})

	// Override the nasServiceIP in the volume context if VERGEOS_NAS_IP is set.
	if nasIP := os.Getenv("VERGEOS_NAS_IP"); nasIP != "" {
		volumeCtx["nasServiceIP"] = nasIP
	}

	publishReq := &csi.NodePublishVolumeRequest{
		VolumeId:      volumeID,
		TargetPath:    targetPath,
		VolumeContext: volumeCtx,
		VolumeCapability: &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			},
		},
	}

	// First publish.
	_, err := node.NodePublishVolume(ctx, publishReq)
	if err != nil {
		t.Fatalf("NodePublishVolume (first call) failed: %v", err)
	}
	t.Log("First NodePublishVolume succeeded")

	// Second publish — should be idempotent (already mounted).
	_, err = node.NodePublishVolume(ctx, publishReq)
	if err != nil {
		t.Fatalf("NodePublishVolume (second call, idempotent) failed: %v", err)
	}
	t.Log("Second NodePublishVolume succeeded (idempotent)")

	// Verify still mounted.
	mounter := util.NewMounter()
	mounted, err := mounter.IsMounted(targetPath)
	if err != nil {
		t.Fatalf("checking mount status: %v", err)
	}
	if !mounted {
		t.Error("target path not mounted after idempotent publish calls")
	}

	// Cleanup: unpublish.
	_, err = node.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
	})
	if err != nil {
		t.Fatalf("cleanup NodeUnpublishVolume failed: %v", err)
	}
	t.Log("Cleanup: NodeUnpublishVolume succeeded")
}

func TestIntegration_NodeUnpublishVolume_NotMounted(t *testing.T) {
	node := newIntegrationNode(t)
	ctx := context.Background()

	// Unpublish a target that was never mounted — should succeed (idempotent).
	targetPath := filepath.Join(t.TempDir(), "not-mounted")

	_, err := node.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "1:fake_vol:fake_share",
		TargetPath: targetPath,
	})
	if err != nil {
		t.Fatalf("NodeUnpublishVolume on non-mounted path should succeed: %v", err)
	}
	t.Log("NodeUnpublishVolume on non-mounted path succeeded (idempotent)")
}
