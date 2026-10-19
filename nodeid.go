package attos

import (
	"os"
	"strings"

	"github.com/google/uuid"
)

// maxNodeIDLen is the longest X-Attos-Node-ID the service accepts.
const maxNodeIDLen = 128

// defaultNodeID is ATTOS_NODE_ID, else the hostname, else a random UUIDv4,
// so a process restarted on the same host keeps its node ID (one active node,
// not two, during a restart). Values longer than 128 characters are cut.
func defaultNodeID() string {
	return nodeIDFrom(os.Getenv("ATTOS_NODE_ID"), os.Hostname)
}

func nodeIDFrom(env string, hostname func() (string, error)) string {
	if v := strings.TrimSpace(env); v != "" {
		return truncateNodeID(v)
	}
	if h, err := hostname(); err == nil && strings.TrimSpace(h) != "" {
		return truncateNodeID(strings.TrimSpace(h))
	}
	return uuid.New().String()
}

func truncateNodeID(v string) string {
	if len(v) > maxNodeIDLen {
		return v[:maxNodeIDLen]
	}
	return v
}
