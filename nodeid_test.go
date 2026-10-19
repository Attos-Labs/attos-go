package attos

import (
	"errors"
	"strings"
	"testing"
)

func TestDefaultNodeID(t *testing.T) {
	host := func(h string, err error) func() (string, error) { return func() (string, error) { return h, err } }
	if got := nodeIDFrom(" pod-1 ", host("ignored", nil)); got != "pod-1" {
		t.Fatalf("env wins: %q", got)
	}
	if got := nodeIDFrom("", host("web-7.internal", nil)); got != "web-7.internal" {
		t.Fatalf("hostname: %q", got)
	}
	if got := nodeIDFrom("", host("", errors.New("no hostname"))); len(got) != 36 {
		t.Fatalf("uuid fallback: %q", got)
	}
	if got := nodeIDFrom(strings.Repeat("x", 300), host("", nil)); len(got) != maxNodeIDLen {
		t.Fatalf("truncated to %d: %d", maxNodeIDLen, len(got))
	}
	t.Setenv("ATTOS_NODE_ID", "from-env")
	f := newFakeServer(t)
	f.set(buildBlob(t, "A", 10), `"a"`)
	s := newTestSync(t, f, t.TempDir())
	mustValue(t, s, "key_1", "A_1")
	if s.NodeID() != "from-env" || f.syncReqs[0].Header.Get("X-Attos-Node-ID") != "from-env" {
		t.Fatalf("node id %q", s.NodeID())
	}
}
