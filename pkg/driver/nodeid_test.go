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
	tests := []struct{ name, providerID, want string }{
		{"capi uuid", "vergeos://" + nodeTestUUID, "uuid:" + nodeTestUUID},
		{"uuid is lowercased", "vergeos://" + strings.ToUpper(nodeTestUUID), "uuid:" + nodeTestUUID},
		{"legacy key", "vergeos://42", "key:42"},
		{"empty", "", "name:node-a"},
		{"other provider", "aws:///i-123", "name:node-a"},
		{"unparseable", "vergeos://garbage", "name:node-a"},
		{"no id", "vergeos://", "name:node-a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NodeIDFromProviderID("node-a", tt.providerID); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
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
		{id: "name:a'b", wantError: true},
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
		want    string
		wantErr bool
	}{
		{"uuid providerID", 200, `{"spec":{"providerID":"vergeos://` + nodeTestUUID + `"}}`, "uuid:" + nodeTestUUID, false},
		{"key providerID", 200, `{"spec":{"providerID":"vergeos://12"}}`, "key:12", false},
		{"no providerID yet", 200, `{"spec":{}}`, "name:node-a", false},
		{"node missing", 404, `{}`, "name:node-a", false},
		{"forbidden", 403, `{"message":"forbidden"}`, "", true},
		{"bad json", 200, `{`, "", true},
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

			r := &KubeNodeIDResolver{NodeName: "node-a", APIServer: srv.URL, TokenFile: tokenFile, HTTP: srv.Client()}
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
