package driver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const nodeTestUUID = "0f3c2a58-7a49-4d51-9d0e-6a3b1c2d4e5f"

func TestNodeIDFromProviderID(t *testing.T) {
	tests := []struct {
		name, providerID, want string
		wantOK                 bool
	}{
		{"capi uuid", "vergeos://" + nodeTestUUID, "uuid:" + nodeTestUUID, true},
		{"uuid is lowercased", "vergeos://" + strings.ToUpper(nodeTestUUID), "uuid:" + nodeTestUUID, true},
		{"legacy key", "vergeos://42", "key:42", true},
		{"empty", "", "name:node-a", false},
		{"other provider", "aws:///i-123", "name:node-a", false},
		{"unparseable", "vergeos://garbage", "name:node-a", false},
		{"no id", "vergeos://", "name:node-a", false},
		{"overflowing key", "vergeos://99999999999999999999999", "name:node-a", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := NodeIDFromProviderID("node-a", tt.providerID)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("got (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestParseNodeID(t *testing.T) {
	tests := []struct {
		id        string
		kind      NodeIDKind
		value     string
		wantError bool
	}{
		{id: "uuid:" + nodeTestUUID, kind: NodeIDKindUUID, value: nodeTestUUID},
		{id: "key:7", kind: NodeIDKindKey, value: "7"},
		{id: "name:vm-1", kind: NodeIDKindName, value: "vm-1"},
		{id: "vm-1", kind: NodeIDKindName, value: "vm-1"}, // legacy bare ID
		{id: "", wantError: true},
		{id: "uuid:nope", wantError: true},
		{id: "key:-1", wantError: true},
		{id: "key:1 or 1", wantError: true},
		{id: "name:", wantError: true},
		{id: "name:a'b\\c", kind: NodeIDKindName, value: "a'b\\c"}, // escaped by the controller, not rejected
	}
	for _, tt := range tests {
		kind, value, err := ParseNodeID(tt.id)
		if tt.wantError {
			if err == nil {
				t.Errorf("%q: expected error", tt.id)
			}
			continue
		}
		if err != nil || kind != tt.kind || value != tt.value {
			t.Errorf("%q: got (%s, %s, %v), want (%s, %s)", tt.id, kind, value, err, tt.kind, tt.value)
		}
	}
}

func TestKubeNodeIDResolver(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		status  int
		body    string
		require bool
		want    string
		wantErr bool
	}{
		{"uuid providerID", 200, `{"spec":{"providerID":"vergeos://` + nodeTestUUID + `"}}`, false, "uuid:" + nodeTestUUID, false},
		{"uuid providerID, required", 200, `{"spec":{"providerID":"vergeos://` + nodeTestUUID + `"}}`, true, "uuid:" + nodeTestUUID, false},
		{"key providerID", 200, `{"spec":{"providerID":"vergeos://12"}}`, false, "key:12", false},
		{"no providerID yet", 200, `{"spec":{}}`, false, "name:node-a", false},
		{"no providerID yet, required", 200, `{"spec":{}}`, true, "", true},
		{"foreign providerID, required", 200, `{"spec":{"providerID":"aws:///x"}}`, true, "", true},
		{"node missing", 404, `{}`, false, "name:node-a", false},
		{"node missing, required", 404, `{}`, true, "", true},
		{"forbidden", 403, `{"message":"forbidden"}`, false, "", true},
		{"bad json", 200, `{`, false, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/nodes/node-a" || r.Header.Get("Authorization") != "Bearer tok" {
					t.Errorf("unexpected request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			r := &KubeNodeIDResolver{NodeName: "node-a", RequireProviderID: tt.require, APIServer: srv.URL, TokenFile: tokenFile, HTTP: srv.Client()}
			got, err := r.ResolveNodeID(context.Background())
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("got (%q, %v), want (%q, err=%v)", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

type fakeResolver struct {
	id  string
	err error
}

func (f fakeResolver) ResolveNodeID(context.Context) (string, error) { return f.id, f.err }

func TestNodeGetInfo_UsesResolver(t *testing.T) {
	ns := NewNodeService("node-a", nil).WithNodeIDResolver(fakeResolver{id: "uuid:" + nodeTestUUID})
	resp, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil || resp.NodeId != "uuid:"+nodeTestUUID {
		t.Errorf("got (%v, %v)", resp, err)
	}
}

func TestNodeGetInfo_ResolverErrorIsUnavailable(t *testing.T) {
	ns := NewNodeService("node-a", nil).WithNodeIDResolver(fakeResolver{err: context.DeadlineExceeded})
	_, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if st, _ := status.FromError(err); st.Code() != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", st.Code())
	}
}

func TestNewInClusterNodeIDResolver_Env(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	if r, err := NewInClusterNodeIDResolver("n", false); r != nil || err != nil {
		t.Errorf("outside a cluster: got (%v, %v), want (nil, nil)", r, err)
	}

	// In a cluster but the CA mount is absent (as in the test environment):
	// must be an error, not a silent fallback.
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	if _, err := NewInClusterNodeIDResolver("n", false); err == nil || !strings.Contains(err.Error(), "cluster CA") {
		t.Errorf("err = %v, want cluster CA error", err)
	}
}

func TestNew_RequireProviderIDOutsideClusterFails(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	_, err := New(Config{DriverType: BlockDriver, Mode: NodeMode, NodeID: "n", RequireProviderID: true})
	if err == nil {
		t.Fatal("expected error: --require-provider-id cannot work outside a cluster")
	}
	if _, err := New(Config{DriverType: BlockDriver, Mode: NodeMode, NodeID: "n"}); err != nil {
		t.Fatalf("without the flag, outside a cluster must fall back: %v", err)
	}
}
