package main

import (
	"strings"
	"testing"

	"github.com/verge-io/csi-vergeos/pkg/driver"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     driver.Config
		pool    int
		wantErr string
	}{
		{"nas controller ok", driver.Config{DriverType: "nas", Mode: "controller", Endpoint: "unix:///x"}, 0, ""},
		{"block controller ok", driver.Config{DriverType: "block", Mode: "controller", Endpoint: "unix:///x"}, 12, ""},
		{"block controller needs pool", driver.Config{DriverType: "block", Mode: "controller", Endpoint: "unix:///x"}, 0, "--pool-vm-id"},
		{"node ok", driver.Config{DriverType: "block", Mode: "node", Endpoint: "unix:///x", NodeID: "n1"}, 0, ""},
		{"node needs id", driver.Config{DriverType: "nas", Mode: "node", Endpoint: "unix:///x"}, 0, "--node-id"},
		{"bad driver", driver.Config{DriverType: "foo", Mode: "node", Endpoint: "unix:///x", NodeID: "n"}, 0, "--driver"},
		{"bad mode", driver.Config{DriverType: "nas", Mode: "both", Endpoint: "unix:///x"}, 0, "--mode"},
		{"empty endpoint", driver.Config{DriverType: "nas", Mode: "controller"}, 0, "--endpoint"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validate(tt.cfg, tt.pool)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
