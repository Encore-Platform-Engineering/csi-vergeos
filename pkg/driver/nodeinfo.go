package driver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"k8s.io/klog/v2"
)

const (
	serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
)

// NodeIDResolver returns the CSI node ID for this node.
type NodeIDResolver interface {
	ResolveNodeID(ctx context.Context) (string, error)
}

// KubeNodeIDResolver reads the Kubernetes Node object for NodeName and derives
// the node ID from spec.providerID. It talks to the API server with the pod's
// service account (needs `get` on `nodes`).
type KubeNodeIDResolver struct {
	NodeName string
	// RequireProviderID makes a missing/unrecognised providerID an error
	// instead of a fallback to `name:<NodeName>`.
	RequireProviderID bool
	// APIServer is the base URL, e.g. https://10.96.0.1:443.
	APIServer string
	// TokenFile is re-read on every call because projected tokens rotate.
	TokenFile string
	HTTP      *http.Client
}

// NewInClusterNodeIDResolver builds a resolver from the pod environment and
// service account mount. It returns (nil, nil) when not running in a cluster
// (KUBERNETES_SERVICE_HOST/PORT unset) and an error when they are set but the
// CA bundle is unreadable or invalid.
func NewInClusterNodeIDResolver(nodeName string, requireProviderID bool) (*KubeNodeIDResolver, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, nil
	}
	ca, err := os.ReadFile(serviceAccountDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("reading cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("cluster CA %s/ca.crt has no valid certificates", serviceAccountDir)
	}
	return &KubeNodeIDResolver{
		NodeName:          nodeName,
		RequireProviderID: requireProviderID,
		APIServer:         "https://" + net.JoinHostPort(host, port),
		TokenFile:         serviceAccountDir + "/token",
		HTTP: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		},
	}, nil
}

// ResolveNodeID returns `uuid:`/`key:` from the Node's providerID. When the
// Node or its providerID is not available yet it returns `name:<NodeName>`
// (with a warning), or an error if RequireProviderID is set so the caller is
// retried. API/auth failures are always errors so the caller does not silently
// register a name that cannot be resolved.
func (r *KubeNodeIDResolver) ResolveNodeID(ctx context.Context) (string, error) {
	token, err := os.ReadFile(r.TokenFile)
	if err != nil {
		return "", fmt.Errorf("reading service account token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		r.APIServer+"/api/v1/nodes/"+url.PathEscape(r.NodeName), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	req.Header.Set("Accept", "application/json")

	resp, err := r.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("getting node %q: %w", r.NodeName, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return r.fallback("node not found")
	case resp.StatusCode != http.StatusOK:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("getting node %q: %s: %s", r.NodeName, resp.Status, body)
	}

	var node struct {
		Spec struct {
			ProviderID string `json:"providerID"`
		} `json:"spec"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&node); err != nil {
		return "", fmt.Errorf("decoding node %q: %w", r.NodeName, err)
	}
	id, ok := NodeIDFromProviderID(r.NodeName, node.Spec.ProviderID)
	if !ok {
		return r.fallback(fmt.Sprintf("providerID %q is empty or not vergeos://", node.Spec.ProviderID))
	}
	return id, nil
}

func (r *KubeNodeIDResolver) fallback(reason string) (string, error) {
	if r.RequireProviderID {
		return "", fmt.Errorf("node %q has no usable providerID yet (%s); is the cloud controller manager running?", r.NodeName, reason)
	}
	klog.Warningf("Node %q: %s; reporting name-based node ID. Attach fails if the VM name differs from the node name (set --require-provider-id to wait instead)", r.NodeName, reason)
	return FormatNodeID(NodeIDKindName, r.NodeName), nil
}
