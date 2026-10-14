package attos

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// reportMetrics flushes one telemetry report and returns its metrics.
func reportMetrics(t *testing.T, s *Synchronizer, f *fakeServer) map[string]int64 {
	t.Helper()
	f.mu.Lock()
	before := len(f.telem)
	f.mu.Unlock()
	if err := s.FlushTelemetry(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.telem) == before+1 })
	f.mu.Lock()
	body := f.telem[before]
	f.mu.Unlock()
	var p otelPayload
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, m := range p.ResourceSpans[0].ScopeMetrics[0].Metrics {
		out[m.Name] = m.Gauge.DataPoints[0].AsInt
	}
	return out
}

// TestTelemetryLatencyPerAccessor (Phase 3 defect fix): every lookup accessor
// feeds the sampled latency histogram, so a client that only uses the safe
// copying accessor GetCopy, or only View, still reports p50/p95/p99 > 0.
func TestTelemetryLatencyPerAccessor(t *testing.T) {
	f := newFakeServer(t)
	f.set(buildBlob(t, "A", 100), `"a"`)
	s := newTestSync(t, f, t.TempDir(), WithTelemetryInterval(time.Hour))
	const n = 50_000
	keys := make([][]byte, 100)
	for i := range keys {
		keys[i] = []byte(fmt.Sprintf("key_%d", i))
	}
	accessors := map[string]func(k []byte){
		"GetCopy": func(k []byte) {
			var buf [32]byte
			if _, err := s.GetCopy(k, buf[:0]); err != nil {
				t.Fatal(err)
			}
		},
		"View": func(k []byte) {
			if err := s.View(k, func([]byte) {}); err != nil {
				t.Fatal(err)
			}
		},
		"GetString": func(k []byte) {
			if _, err := s.GetString(string(k)); err != nil {
				t.Fatal(err)
			}
		},
		"Get": func(k []byte) {
			if _, err := s.Get(k); err != nil {
				t.Fatal(err)
			}
		},
	}
	for _, name := range []string{"GetCopy", "View", "GetString", "Get"} {
		reportMetrics(t, s, f) // reset the window
		for i := 0; i < n; i++ {
			accessors[name](keys[i%len(keys)])
		}
		m := reportMetrics(t, s, f)
		if m["attos.lookups_count"] != n {
			t.Fatalf("%s: lookups_count=%d want %d", name, m["attos.lookups_count"], n)
		}
		if m["attos.lookup.p50_ns"] <= 0 || m["attos.lookup.p95_ns"] <= 0 || m["attos.lookup.p99_ns"] <= 0 ||
			m["attos.lookup.p99_ns"] < m["attos.lookup.p50_ns"] {
			t.Fatalf("%s: latency percentiles not populated: %v", name, m)
		}
		t.Logf("%s only: lookups=%d p50=%d p95=%d p99=%d", name, m["attos.lookups_count"],
			m["attos.lookup.p50_ns"], m["attos.lookup.p95_ns"], m["attos.lookup.p99_ns"])
	}
}
