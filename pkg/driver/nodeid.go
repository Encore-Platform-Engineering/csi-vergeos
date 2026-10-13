package driver

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// NodeIDKind says how a CSI node ID identifies the VergeOS VM backing a node.
type NodeIDKind string

const (
	// NodeIDKindUUID is `uuid:<vm uuid>`; matches vms.uuid.
	NodeIDKindUUID NodeIDKind = "uuid"
	// NodeIDKindKey is `key:<n>`; matches the VM $key.
	NodeIDKindKey NodeIDKind = "key"
	// NodeIDKindName is `name:<vm name>`; matches the VM name. A bare string
	// with no recognised prefix (IDs from older installs) is also a name.
	NodeIDKindName NodeIDKind = "name"
)

const providerIDPrefix = "vergeos://"

var (
	uuidRe   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	digitsRe = regexp.MustCompile(`^[0-9]+$`)
)

// FormatNodeID builds a CSI node ID from a kind and value.
func FormatNodeID(kind NodeIDKind, value string) string {
	return string(kind) + ":" + value
}

// ParseNodeID splits a CSI node ID into kind and value, validating the value
// so it is safe to embed in a VergeOS filter expression.
func ParseNodeID(nodeID string) (NodeIDKind, string, error) {
	if nodeID == "" {
		return "", "", fmt.Errorf("empty node ID")
	}
	kind, value := NodeIDKindName, nodeID
	if k, v, ok := strings.Cut(nodeID, ":"); ok {
		switch NodeIDKind(k) {
		case NodeIDKindUUID, NodeIDKindKey, NodeIDKindName:
			kind, value = NodeIDKind(k), v
		}
	}
	switch kind {
	case NodeIDKindUUID:
		if !uuidRe.MatchString(value) {
			return "", "", fmt.Errorf("node ID %q: invalid VM UUID", nodeID)
		}
		value = strings.ToLower(value)
	case NodeIDKindKey:
		n, err := strconv.Atoi(value)
		if err != nil || !digitsRe.MatchString(value) {
			return "", "", fmt.Errorf("node ID %q: invalid VM key", nodeID)
		}
		value = strconv.Itoa(n)
	case NodeIDKindName:
		if value == "" || strings.ContainsAny(value, "'\\") {
			return "", "", fmt.Errorf("node ID %q: invalid VM name", nodeID)
		}
	}
	return kind, value, nil
}

// NodeIDFromProviderID derives a CSI node ID from a Node's spec.providerID.
// Accepts `vergeos://<uuid>` (Cluster API) and `vergeos://<int $key>` (legacy
// CCM). Anything else, including empty, falls back to `name:<nodeName>`.
func NodeIDFromProviderID(nodeName, providerID string) string {
	if rest, ok := strings.CutPrefix(providerID, providerIDPrefix); ok {
		switch {
		case uuidRe.MatchString(rest):
			return FormatNodeID(NodeIDKindUUID, strings.ToLower(rest))
		case digitsRe.MatchString(rest):
			return FormatNodeID(NodeIDKindKey, rest)
		}
	}
	return FormatNodeID(NodeIDKindName, nodeName)
}
