package attos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func kvBlob(t testing.TB, n int) string {
	t.Helper()
	var in strings.Builder
	in.WriteString("id,attos_value\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&in, "k%d,v%d\n", i, i)
	}
	var out bytes.Buffer
	if _, err := Compile(context.Background(), strings.NewReader(in.String()), FormatAuto, &out); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "b.attos")
	if err := os.WriteFile(p, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGetManyMatchesGetAndCountsMisses(t *testing.T) {
	m, err := Open(kvBlob(t, 1000))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	keys := make([][]byte, 2000)
	for i := range keys {
		keys[i] = []byte(fmt.Sprintf("k%d", i))
	}
	out := make([][]byte, len(keys))
	found, err := m.GetMany(keys, out)
	if err != nil || found != 1000 {
		t.Fatalf("found=%d err=%v", found, err)
	}
	for i, v := range out {
		if i < 1000 && string(v) != fmt.Sprintf("v%d", i) {
			t.Fatalf("key %d: %q", i, v)
		}
		if i >= 1000 && v != nil {
			t.Fatalf("key %d should be absent, got %q", i, v)
		}
	}
	if m.Lookups() != 2000 || m.Misses() != 1000 {
		t.Fatalf("lookups=%d misses=%d", m.Lookups(), m.Misses())
	}
	if _, err := m.Get([]byte("nope")); err == nil || m.Misses() != 1001 {
		t.Fatalf("single-get miss not counted: %d", m.Misses())
	}
	if _, err := m.GetMany(keys, out[:5]); !errors.Is(err, ErrBatchOutTooSmall) {
		t.Fatalf("want ErrBatchOutTooSmall, got %v", err)
	}
	if n, err := m.GetMany(nil, nil); n != 0 || err != nil {
		t.Fatal(n, err)
	}
}

func TestGetManyZeroAllocs(t *testing.T) {
	m, err := Open(kvBlob(t, 500))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	keys := make([][]byte, 64)
	for i := range keys {
		keys[i] = []byte(fmt.Sprintf("k%d", i))
	}
	out := make([][]byte, len(keys))
	if a := testing.AllocsPerRun(100, func() { _, _ = m.GetMany(keys, out) }); a != 0 {
		t.Fatalf("GetMany allocated %v times per run", a)
	}
}

func TestMetricsSurviveDrainAndSwap(t *testing.T) {
	m, err := Open(kvBlob(t, 100))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for r := 0; r < 100; r++ {
		for i := 0; i < 1000; i++ {
			_, _ = m.Get([]byte(fmt.Sprintf("k%d", i)))
		}
	}
	sum := func() (n uint64) {
		for _, b := range m.LatencyBuckets() {
			n += b.Count
		}
		return
	}
	before := sum()
	if before == 0 {
		t.Fatal("no sampled latencies")
	}
	m.DrainLatency()
	if sum() != before {
		t.Fatal("drain reduced cumulative buckets")
	}
	if err := m.Reload(kvBlob(t, 100)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return m.PendingRetired() == 0 })
	if sum() != before || m.Lookups() != 100000 || m.Misses() != 90000 {
		t.Fatalf("buckets=%d lookups=%d misses=%d", sum(), m.Lookups(), m.Misses())
	}
	var b strings.Builder
	m.WritePrometheus(&b)
	for _, want := range []string{
		"attos_lookups_total 100000\n", "attos_lookup_misses_total 90000\n", "attos_lookup_hit_ratio 0.1\n",
		`attos_file_info{format_version="`, `attos_lookup_duration_seconds_bucket{le="+Inf"}`,
	} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("missing %q in\n%s", want, b.String())
		}
	}
	rec := httptest.NewRecorder()
	MetricsHandler(m).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "attos_lookups_total") {
		t.Fatalf("handler: %d", rec.Code)
	}
}

func TestSchemaRoundTrip(t *testing.T) {
	sc := NewSchema(Field{"id", U64}, Field{"score", F64}, Field{"active", Bool}, Field{"name", String}, Field{"delta", I16})
	enc, err := sc.Encode(uint64(42), 1.5, true, "héllo", int16(-7))
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := sc.Uint(enc, "id"); n != 42 {
		t.Fatal(n)
	}
	if f, _ := sc.Float(enc, "score"); f != 1.5 {
		t.Fatal(f)
	}
	if b, _ := sc.Bool(enc, "active"); !b {
		t.Fatal("bool")
	}
	if s, _ := sc.String(enc, "name"); s != "héllo" {
		t.Fatal(s)
	}
	if i, _ := sc.Int(enc, "delta"); i != -7 {
		t.Fatal(i)
	}
	if _, err := sc.String(enc, "missing"); !errors.Is(err, ErrSchema) {
		t.Fatal(err)
	}
	if _, err := sc.Int(enc, "id"); !errors.Is(err, ErrSchema) {
		t.Fatal(err)
	}
	if _, err := sc.Int(enc[:len(enc)-1], "delta"); !errors.Is(err, ErrSchema) {
		t.Fatal(err)
	}
	if _, err := NewSchema(Field{"x", U8}).Encode(uint64(300)); !errors.Is(err, ErrSchema) {
		t.Fatal(err)
	}
}
