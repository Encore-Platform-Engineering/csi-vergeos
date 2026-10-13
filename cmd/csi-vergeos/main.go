// Command csi-vergeos runs the VergeOS CSI driver (NAS or block, controller or node).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	vergeos "github.com/verge-io/govergeos"
	"k8s.io/klog/v2"

	"github.com/verge-io/csi-vergeos/pkg/block"
	"github.com/verge-io/csi-vergeos/pkg/driver"
	"github.com/verge-io/csi-vergeos/pkg/nas"
)

func main() {
	klog.InitFlags(nil) // registers --v and friends on flag.CommandLine

	var (
		driverType = flag.String("driver", "", "driver type: nas or block")
		mode       = flag.String("mode", "", "service mode: controller or node")
		endpoint   = flag.String("endpoint", "unix:///csi/csi.sock", "CSI gRPC endpoint (unix:// only)")
		nodeID     = flag.String("node-id", "", "node ID reported by the node service (node mode)")
		reqProvID  = flag.Bool("require-provider-id", false, "block node: wait for Node.spec.providerID (vergeos://) instead of falling back to the Node name")
		poolVMID   = flag.Int("pool-vm-id", 0, "VergeOS VM $key that holds unattached drives (block controller)")
		version    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	defer klog.Flush()

	if *version {
		fmt.Println(driver.DriverVersion)
		return
	}

	if err := run(*driverType, *mode, *endpoint, *nodeID, *poolVMID, *reqProvID); err != nil {
		klog.Errorf("csi-vergeos: %v", err)
		klog.Flush()
		os.Exit(1)
	}
}

func run(driverType, mode, endpoint, nodeID string, poolVMID int, requireProviderID bool) error {
	cfg := driver.Config{
		DriverType:        driver.DriverType(driverType),
		Mode:              driver.DriverMode(mode),
		Endpoint:          endpoint,
		NodeID:            nodeID,
		RequireProviderID: requireProviderID,
	}
	if err := validate(cfg, poolVMID); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch cfg.Mode {
	case driver.ControllerMode:
		client, err := vergeos.NewClient(vergeos.WithEnvConfig())
		if err != nil {
			return fmt.Errorf("creating VergeOS client: %w", err)
		}
		switch cfg.DriverType {
		case driver.NASDriver:
			cfg.ControllerBackend = nas.NewNASController(client)
		case driver.BlockDriver:
			bc, err := block.NewBlockController(ctx, client, poolVMID)
			if err != nil {
				return fmt.Errorf("creating block controller: %w", err)
			}
			cfg.ControllerBackend = bc
		}
	case driver.NodeMode:
		switch cfg.DriverType {
		case driver.NASDriver:
			cfg.NodeBackend = nas.NewNASNode()
		case driver.BlockDriver:
			cfg.NodeBackend = block.NewBlockNode()
		}
	}

	d, err := driver.New(cfg)
	if err != nil {
		return fmt.Errorf("creating driver: %w", err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run() }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		klog.Infof("Received shutdown signal")
		d.Stop()
		return nil
	}
}

// validate checks flag combinations before any backend is built.
func validate(cfg driver.Config, poolVMID int) error {
	if driver.DriverNameForType(cfg.DriverType) == "" {
		return fmt.Errorf("--driver must be %q or %q, got %q", driver.NASDriver, driver.BlockDriver, cfg.DriverType)
	}
	switch cfg.Mode {
	case driver.ControllerMode:
		if cfg.DriverType == driver.BlockDriver && poolVMID <= 0 {
			return fmt.Errorf("--pool-vm-id is required for --driver=block --mode=controller")
		}
	case driver.NodeMode:
		if cfg.NodeID == "" {
			return fmt.Errorf("--node-id is required for --mode=node")
		}
	default:
		return fmt.Errorf("--mode must be %q or %q, got %q", driver.ControllerMode, driver.NodeMode, cfg.Mode)
	}
	if cfg.Endpoint == "" {
		return fmt.Errorf("--endpoint is required")
	}
	return nil
}
