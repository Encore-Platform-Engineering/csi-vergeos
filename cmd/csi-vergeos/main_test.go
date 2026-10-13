package main

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	vergeos "github.com/verge-io/govergeos"

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

// The chart mounts a private CA and sets SSL_CERT_FILE; this proves the SDK
// client built by main (WithEnvConfig, VERIFY_SSL unset) trusts it. Go reads
// SSL_CERT_FILE once per process, so the check runs in a child process.
func TestSSLCertFileTrustedBySDK(t *testing.T) {
	if os.Getenv("CSI_TEST_TLS_URL") != "" {
		return // child mode: see TestSSLCertFileChild
	}
	if runtime.GOOS != "linux" {
		t.Skip("Go honours SSL_CERT_FILE only on Linux (the container platform)")
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"26.0.0"}`))
	}))
	defer srv.Close()
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	run := func(sslCertFile string) (string, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSSLCertFileChild$")
		cmd.Env = append(os.Environ(),
			"CSI_TEST_TLS_URL="+srv.URL,
			"VERGEOS_HOST="+srv.URL,
			"VERGEOS_API_KEY=k",
			"SSL_CERT_FILE="+sslCertFile,
		)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run(caFile); err != nil {
		t.Fatalf("with SSL_CERT_FILE=ca.crt the client must trust the server: %v\n%s", err, out)
	}
	if out, err := run(os.DevNull); err == nil {
		t.Fatalf("without the CA the client must reject the server certificate\n%s", out)
	}
}

func TestSSLCertFileChild(t *testing.T) {
	if os.Getenv("CSI_TEST_TLS_URL") == "" {
		t.Skip("child helper")
	}
	// NewClient fetches /version.json over TLS, so success proves trust.
	if _, err := vergeos.NewClient(vergeos.WithEnvConfig()); err != nil {
		t.Fatal(err)
	}
}
