//go:build integration

package nas

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	vergeos "github.com/verge-io/govergeos"
)

const (
	// testNASServiceName is the NAS service to use for integration tests.
	// This must exist on the test system before running tests.
	testNASServiceName = "Services 26.1.2-0"

	// testNASServiceIP is a static IP for the NAS service. This is used
	// when the NAS service VM doesn't have a dynamically-discoverable IP
	// (e.g., the VM is powered off or has no assigned VNet address).
	// The controller only stores this in the volume context; it doesn't
	// connect to it during CreateVolume.
	testNASServiceIP = "10.0.0.5"
)

func newIntegrationController(t *testing.T) *NASController {
	t.Helper()

	// Ensure required env vars are set.
	for _, env := range []string{"VERGEOS_HOST", "VERGEOS_USERNAME", "VERGEOS_PASSWORD"} {
		if os.Getenv(env) == "" {
			t.Skipf("skipping integration test: %s not set", env)
		}
	}

	client, err := vergeos.NewClient(vergeos.WithEnvConfig())
	if err != nil {
		t.Fatalf("creating VergeOS client: %v", err)
	}

	return NewNASController(client)
}

func TestIntegration_CreateAndDeleteVolume(t *testing.T) {
	c := newIntegrationController(t)
	ctx := context.Background()

	volumeName := fmt.Sprintf("csi-test-%d", time.Now().UnixNano())

	// Ensure cleanup even on failure.
	var volumeID string
	t.Cleanup(func() {
		if volumeID != "" {
			_, err := c.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: volumeID})
			if err != nil {
				t.Logf("cleanup: DeleteVolume(%s) failed: %v", volumeID, err)
			}
		}
	})

	// --- CreateVolume ---
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
	t.Logf("Created volume: %s", volumeID)

	// Verify volume context contains expected fields.
	vc := resp.Volume.VolumeContext
	if vc["nasServiceName"] != testNASServiceName {
		t.Errorf("VolumeContext nasServiceName = %q, want %q", vc["nasServiceName"], testNASServiceName)
	}
	if vc["volumeID"] == "" {
		t.Error("VolumeContext volumeID is empty")
	}
	if vc["nfsShareID"] == "" {
		t.Error("VolumeContext nfsShareID is empty")
	}
	if vc["nasServiceIP"] != testNASServiceIP {
		t.Errorf("VolumeContext nasServiceIP = %q, want %q", vc["nasServiceIP"], testNASServiceIP)
	}
	if resp.Volume.CapacityBytes != 1073741824 {
		t.Errorf("CapacityBytes = %d, want %d", resp.Volume.CapacityBytes, 1073741824)
	}

	// --- DeleteVolume ---
	_, err = c.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: volumeID})
	if err != nil {
		t.Fatalf("DeleteVolume failed: %v", err)
	}
	t.Logf("Deleted volume: %s", volumeID)

	// Clear volumeID so cleanup doesn't try to delete again.
	volumeID = ""
}

func TestIntegration_CreateVolume_Idempotent(t *testing.T) {
	c := newIntegrationController(t)
	ctx := context.Background()

	volumeName := fmt.Sprintf("csi-test-%d", time.Now().UnixNano())

	var volumeID string
	t.Cleanup(func() {
		if volumeID != "" {
			_, err := c.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: volumeID})
			if err != nil {
				t.Logf("cleanup: DeleteVolume(%s) failed: %v", volumeID, err)
			}
		}
	})

	createReq := &csi.CreateVolumeRequest{
		Name: volumeName,
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1073741824,
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

	// First create.
	resp1, err := c.CreateVolume(ctx, createReq)
	if err != nil {
		t.Fatalf("CreateVolume (first call) failed: %v", err)
	}
	volumeID = resp1.Volume.VolumeId
	t.Logf("First CreateVolume returned: %s", volumeID)

	// Second create — should be idempotent.
	resp2, err := c.CreateVolume(ctx, createReq)
	if err != nil {
		t.Fatalf("CreateVolume (second call) failed: %v", err)
	}
	t.Logf("Second CreateVolume returned: %s", resp2.Volume.VolumeId)

	if resp1.Volume.VolumeId != resp2.Volume.VolumeId {
		t.Errorf("idempotent CreateVolume returned different IDs: %q vs %q",
			resp1.Volume.VolumeId, resp2.Volume.VolumeId)
	}
}

func TestIntegration_DeleteVolume_AlreadyGone(t *testing.T) {
	c := newIntegrationController(t)
	ctx := context.Background()

	// Delete a volume that doesn't exist — should succeed (idempotent).
	_, err := c.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
		VolumeId: "1:nonexistent_volume_id:nonexistent_share_id",
	})
	if err != nil {
		t.Fatalf("DeleteVolume of non-existent volume should succeed: %v", err)
	}
	t.Log("DeleteVolume of non-existent volume succeeded (idempotent)")
}
