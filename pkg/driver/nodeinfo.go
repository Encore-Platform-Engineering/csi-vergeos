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
	// APIServer is the base URL, e.g. https://10.96.0.1:443.
	APIServer string
	// TokenFile is re-read on every call because projected tokens rotate.
	TokenFile string
	HTTP      *http.Client
}

// NewInClusterNodeIDResolver builds a resolver from the pod environment and
// service account mount. It returns nil, false when not running in a cluster.
func NewInClusterNodeIDResolver(nodeName string) (*KubeNodeIDResolver, bool) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, false
	}
	ca, err := os.ReadFile(serviceAccountDir + "/ca.crt")
	if err != nil {
		return nil, false
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, false
	}
	return &KubeNodeIDResolver{
		NodeName:  nodeName,
		APIServer: "https://" + net.JoinHostPort(host, port),
		TokenFile: serviceAccountDir + "/token",
		HTTP: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		},
	}, true
}

// ResolveNodeID returns `uuid:`/`key:` from the Node's providerID, or
// `name:<NodeName>` when the Node or its providerID is not available yet.
// API/auth failures are returned as errors so the caller does not silently
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
		return FormatNodeID(NodeIDKindName, r.NodeName), nil
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
	return NodeIDFromProviderID(r.NodeName, node.Spec.ProviderID), nil
}
