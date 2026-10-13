package driver

import (
	"fmt"
	"net"
	"net/url"
	"os"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"
)

// Config holds all configuration needed to create a Driver.
type Config struct {
	DriverType        DriverType
	Mode              DriverMode
	Endpoint          string
	NodeID            string
	ControllerBackend ControllerBackend
	NodeBackend       NodeBackend
	// NodeIDResolver derives the CSI node ID in node mode. When nil, the
	// in-cluster Kubernetes API is used if available, otherwise NodeID is
	// reported as is.
	NodeIDResolver NodeIDResolver
	// RequireProviderID (block node mode): NodeGetInfo fails with Unavailable
	// until the Node has a vergeos:// providerID, instead of falling back to
	// the Node name. Ignored when NodeIDResolver is set.
	RequireProviderID bool
}

// Driver is the top-level CSI driver. It owns the gRPC server and all services.
type Driver struct {
	config     Config
	grpcServer *grpc.Server
}

// New creates a new Driver with the given configuration.
func New(cfg Config) (*Driver, error) {
	driverName := DriverNameForType(cfg.DriverType)
	if driverName == "" {
		return nil, fmt.Errorf("unknown driver type: %q", cfg.DriverType)
	}

	d := &Driver{config: cfg}
	d.grpcServer = grpc.NewServer()

	// Always register Identity service.
	identity := NewIdentityService(driverName, DriverVersion)
	csi.RegisterIdentityServer(d.grpcServer, identity)

	// Register Controller service in controller mode.
	if cfg.Mode == ControllerMode {
		controller := NewControllerService(cfg.ControllerBackend)
		csi.RegisterControllerServer(d.grpcServer, controller)
		klog.Infof("Registered Controller service (driver=%s)", driverName)
	}

	// Register Node service in node mode.
	if cfg.Mode == NodeMode {
		node := NewNodeService(cfg.NodeID, cfg.NodeBackend)
		// Only the block driver maps node IDs to VMs; NAS keeps the plain name.
		resolver := cfg.NodeIDResolver
		if resolver == nil && cfg.DriverType == BlockDriver {
			r, err := NewInClusterNodeIDResolver(cfg.NodeID, cfg.RequireProviderID)
			if err != nil {
				return nil, fmt.Errorf("in-cluster node ID resolver: %w", err)
			}
			if r == nil {
				if cfg.RequireProviderID {
					return nil, fmt.Errorf("--require-provider-id needs the in-cluster Kubernetes API (KUBERNETES_SERVICE_HOST/PORT unset)")
				}
				klog.Warning("Not running in a cluster (KUBERNETES_SERVICE_HOST/PORT unset); node ID is not derived from providerID")
			} else {
				resolver = r
			}
		}
		if resolver != nil {
			node.WithNodeIDResolver(resolver)
		}
		csi.RegisterNodeServer(d.grpcServer, node)
		klog.Infof("Registered Node service (driver=%s, node=%s)", driverName, cfg.NodeID)
	}

	return d, nil
}

// Run starts the gRPC server and blocks until it is stopped.
func (d *Driver) Run() error {
	u, err := url.Parse(d.config.Endpoint)
	if err != nil {
		return fmt.Errorf("parsing endpoint %q: %w", d.config.Endpoint, err)
	}

	var addr string
	if u.Scheme == "unix" {
		addr = u.Path
		// Remove existing socket file if present.
		if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing existing socket %q: %w", addr, err)
		}
	} else {
		return fmt.Errorf("unsupported endpoint scheme %q (only unix:// is supported)", u.Scheme)
	}

	listener, err := net.Listen("unix", addr)
	if err != nil {
		return fmt.Errorf("listening on %q: %w", addr, err)
	}

	driverName := DriverNameForType(d.config.DriverType)
	klog.Infof("gRPC server listening on %s (driver=%s, mode=%s)", addr, driverName, d.config.Mode)

	return d.grpcServer.Serve(listener)
}

// Stop gracefully stops the gRPC server.
func (d *Driver) Stop() {
	klog.Info("Stopping gRPC server")
	d.grpcServer.GracefulStop()
}
