package attos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// buildBlob compiles rows key_i -> "<tag>_i" with the SDK builder.
func buildBlob(t testing.TB, tag string, n int) []byte {
	t.Helper()
	var in strings.Builder
	in.WriteString("id,attos_value\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&in, "key_%d,%s_%d\n", i, tag, i)
	}
	var out bytes.Buffer
	if _, err := Compile(context.Background(), strings.NewReader(in.String()), FormatAuto, &out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// fakeServer is a scripted sync + telemetry endpoint.
type fakeServer struct {
	t  testing.TB
	mu sync.Mutex
	// handler for the next sync request; nil = serve current blob with ETag.
	script   []func(w http.ResponseWriter, r *http.Request)
	blob     []byte
	etag     string
	config   string
	syncReqs []*http.Request
	served   int // number of 200 responses with a body
	telem    [][]byte
	telemHdr []http.Header
	telemFn  func(w http.ResponseWriter, r *http.Request, body []byte) bool
	srv      *httptest.Server
}

func newFakeServer(t testing.TB) *fakeServer {
	f := &fakeServer{t: t, config: `{"telemetry_enabled":true}`}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) set(blob []byte, etag string) {
	f.mu.Lock()
	f.blob, f.etag = blob, etag
	f.mu.Unlock()
}

func (f *fakeServer) push(h ...func(w http.ResponseWriter, r *http.Request)) {
	f.mu.Lock()
	f.script = append(f.script, h...)
	f.mu.Unlock()
}

func (f *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v1/telemetry/report" {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.telem = append(f.telem, body)
		f.telemHdr = append(f.telemHdr, r.Header.Clone())
		fn := f.telemFn
		f.mu.Unlock()
		if fn != nil && fn(w, r, body) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/v1/datasets/sync/") {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	f.syncReqs = append(f.syncReqs, r.Clone(context.Background()))
	var h func(w http.ResponseWriter, r *http.Request)
	if len(f.script) > 0 {
		h, f.script = f.script[0], f.script[1:]
	}
	blob, etag, cfg := f.blob, f.etag, f.config
	f.mu.Unlock()
	if h != nil {
		h(w, r)
		return
	}
	if etag != "" && r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Attos-Config", cfg)
	w.Header().Set("X-Dataset-Name", "fake-dataset")
	w.Header().Set("Content-Length", fmt.Sprint(len(blob)))
	w.Write(blob)
	f.mu.Lock()
	f.served++
	f.mu.Unlock()
}

func (f *fakeServer) counts() (reqs, served int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.syncReqs), f.served
}

func statusHandler(code int, hdr map[string]string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.WriteHeader(code)
		io.WriteString(w, http.StatusText(code))
	}
}

// truncatedHandler declares the full length but sends only half the body.
func truncatedHandler(blob []byte) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(blob)))
		w.Header().Set("ETag", "trunc")
		w.Write(blob[:len(blob)/2])
		// Returning with fewer bytes than Content-Length makes net/http close
		// the connection: the client sees an unexpected EOF.
	}
}

// killHandler sends part of the body and then kills the TCP connection.
func killHandler(blob []byte) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			panic("no hijacker")
		}
		conn, buf, _ := hj.Hijack()
		fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nETag: \"killed\"\r\n\r\n", len(blob))
		buf.Write(blob[:len(blob)/3])
		buf.Flush()
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetLinger(0) // RST
		}
		conn.Close()
	}
}

func corruptCRC(blob []byte) []byte {
	b := append([]byte(nil), blob...)
	b[len(b)-6] ^= 0xFF
	return b
}

func newTestSync(t testing.TB, f *fakeServer, dir string, opts ...Option) *Synchronizer {
	t.Helper()
	all := append([]Option{
		WithBaseURL(f.srv.URL), WithCacheDir(dir), WithAPIKey("test-key"),
		WithSyncInterval(0), WithTelemetryInterval(0), WithLogger(testLogger(t)),
	}, opts...)
	s, err := NewSynchronizer(context.Background(), "ds-1", all...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustValue(t testing.TB, s *Synchronizer, key, want string) {
	t.Helper()
	v, err := s.GetCopy([]byte(key), nil)
	if err != nil || string(v) != want {
		t.Fatalf("Get(%s) = %q, %v; want %q", key, v, err, want)
	}
}

func tempFiles(t testing.TB, dir string) []string {
	m, _ := filepath.Glob(filepath.Join(dir, ".*download*"))
	return m
}

func TestSyncHeadersETagAnd304(t *testing.T) {
	f := newFakeServer(t)
	f.set(buildBlob(t, "A", 100), `"etag-a"`)
	dir := t.TempDir()
	s := newTestSync(t, f, dir, WithNodeID("node-123"))
	mustValue(t, s, "key_7", "A_7")
	if s.DatasetName() != "fake-dataset" || s.ETag() != `"etag-a"` {
		t.Fatalf("name=%q etag=%q", s.DatasetName(), s.ETag())
	}
	r0 := f.syncReqs[0]
	if r0.Header.Get("X-API-Key") != "test-key" || r0.Header.Get("X-Attos-Node-ID") != "node-123" ||
		r0.Header.Get("User-Agent") != "attos-go/"+Version || r0.Header.Get("If-None-Match") != "" {
		t.Fatalf("bad first request headers: %v", r0.Header)
	}
	// Unchanged → 304, no re-download, map untouched.
	before := s.Map().Swaps()
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	reqs, served := f.counts()
	if reqs != 2 || served != 1 || f.syncReqs[1].Header.Get("If-None-Match") != `"etag-a"` {
		t.Fatalf("reqs=%d served=%d inm=%q", reqs, served, f.syncReqs[1].Header.Get("If-None-Match"))
	}
	if s.Map().Swaps() != before {
		t.Fatal("304 must not swap")
	}
	// Changed → 200 → swap.
	f.set(buildBlob(t, "B", 100), `"etag-b"`)
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustValue(t, s, "key_7", "B_7")
	if s.ETag() != `"etag-b"` || s.Map().Swaps() != before+1 {
		t.Fatalf("etag=%q swaps=%d", s.ETag(), s.Map().Swaps())
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "ds-1.etag")); string(b) != `"etag-b"` {
		t.Fatalf("persisted etag %q", b)
	}
	if len(tempFiles(t, dir)) != 0 {
		t.Fatal("temp files left behind")
	}
}

// TestSyncFaultInjection: truncated body, killed connection, corrupted CRC,
// unknown version, 402/429/500 — the SDK keeps serving the old map, leaves no
// temp files, and recovers on the next good response.
func TestSyncFaultInjection(t *testing.T) {
	f := newFakeServer(t)
	good := buildBlob(t, "A", 200)
	f.set(good, `"a"`)
	dir := t.TempDir()
	s := newTestSync(t, f, dir)
	mustValue(t, s, "key_42", "A_42")

	next := buildBlob(t, "B", 200)
	badVersion := append([]byte(nil), next...)
	badVersion[7] = 3
	cases := []struct {
		name string
		h    func(http.ResponseWriter, *http.Request)
		want func(error) bool
	}{
		{"truncated", truncatedHandler(next), func(e error) bool { return e != nil }},
		{"killed-mid-download", killHandler(next), func(e error) bool { return e != nil }},
		{"bad-crc", func(w http.ResponseWriter, r *http.Request) { w.Header().Set("ETag", "x"); w.Write(corruptCRC(next)) },
			func(e error) bool { return errors.Is(e, ErrChecksumMismatch) }},
		{"bad-version", func(w http.ResponseWriter, r *http.Request) { w.Write(badVersion) },
			func(e error) bool { return errors.Is(e, ErrInvalidVersion) }},
		{"garbage", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>oops</html>")) },
			func(e error) bool { return errors.Is(e, ErrInvalidMagic) }},
		{"402", statusHandler(402, nil), func(e error) bool { var se *StatusError; return errors.As(e, &se) && se.Quota() }},
		{"429-retry-after", statusHandler(429, map[string]string{"Retry-After": "7"}),
			func(e error) bool { var se *StatusError; return errors.As(e, &se) && se.RetryAfter == 7*time.Second }},
		{"500", statusHandler(500, nil), func(e error) bool { var se *StatusError; return errors.As(e, &se) && se.Code == 500 }},
		{"404", statusHandler(404, nil), func(e error) bool { var se *StatusError; return errors.As(e, &se) && se.Code == 404 }},
	}
	for _, c := range cases {
		f.push(c.h)
		err := s.Sync(context.Background())
		if !c.want(err) {
			t.Fatalf("%s: unexpected error %v", c.name, err)
		}
		mustValue(t, s, "key_42", "A_42") // still serving the old map
		if tf := tempFiles(t, dir); len(tf) != 0 {
			t.Fatalf("%s: temp files left: %v", c.name, tf)
		}
		t.Logf("%s -> %v (still serving A)", c.name, err)
	}
	if s.crcErrors.Load() != 1 {
		t.Fatalf("crc errors = %d, want 1", s.crcErrors.Load())
	}
	// The live file on disk is still the good blob.
	if b, _ := os.ReadFile(filepath.Join(dir, "ds-1.attos")); !bytes.Equal(b, good) {
		t.Fatal("live blob on disk was modified by a failed sync")
	}
	// Recovery.
	f.set(next, `"b"`)
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustValue(t, s, "key_42", "B_42")
}

func TestSyncContextCancelMidDownload(t *testing.T) {
	f := newFakeServer(t)
	f.set(buildBlob(t, "A", 50), `"a"`)
	dir := t.TempDir()
	s := newTestSync(t, f, dir)
	next := buildBlob(t, "B", 50)
	started := make(chan struct{})
	release := make(chan struct{})
	f.push(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(next)))
		w.Write(next[:10])
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Sync(ctx) }()
	<-started
	cancel()
	err := <-done
	close(release)
	if err == nil {
		t.Fatal("cancelled sync succeeded")
	}
	mustValue(t, s, "key_3", "A_3")
	if tf := tempFiles(t, dir); len(tf) != 0 {
		t.Fatalf("temp files left: %v", tf)
	}
}

func TestSyncBackoffPolicy(t *testing.T) {
	s := &Synchronizer{syncInterval: 30 * time.Second, loggedStatus: map[int]bool{}, logger: testLogger(t)}
	if d := s.nextDelay(&StatusError{Code: 402}); d != QuotaPause {
		t.Fatalf("402 -> %v", d)
	}
	if d := s.nextDelay(&StatusError{Code: 429, RetryAfter: 90 * time.Second}); d != 90*time.Second {
		t.Fatalf("429 RA 90s -> %v", d)
	}
	if d := s.nextDelay(&StatusError{Code: 429, RetryAfter: 3 * time.Hour}); d != MaxRetryAfter {
		t.Fatalf("429 RA 3h -> %v", d)
	}
	for _, c := range []int{400, 401, 403, 404, 409} {
		if d := s.nextDelay(&StatusError{Code: c}); d != s.syncInterval {
			t.Fatalf("%d -> %v", c, d)
		}
	}
	// Transient: 1s·2^k ±20%, capped at min(interval, 5 min).
	s.failures = 0
	for k := 0; k < 8; k++ {
		d := s.nextDelay(&StatusError{Code: 503})
		base := time.Second << k
		if base > s.syncInterval {
			base = s.syncInterval
		}
		lo, hi := time.Duration(float64(base)*0.8), time.Duration(float64(base)*1.2)
		if d < lo || d > hi {
			t.Fatalf("k=%d: %v not in [%v, %v]", k, d, lo, hi)
		}
	}
	if d := s.nextDelay(nil); d != s.syncInterval || s.failures != 0 {
		t.Fatalf("success must reset backoff: %v %d", d, s.failures)
	}
	if d := transientBackoff(30, time.Hour); d > time.Duration(float64(maxTransientBackoff)*1.2) {
		t.Fatalf("cap exceeded: %v", d)
	}
	if parseRetryAfter("12") != 12*time.Second || parseRetryAfter("") != 0 || parseRetryAfter("junk") != 0 {
		t.Fatal("parseRetryAfter")
	}
}

// TestSyncPollLoopRecovers: background loop rides through 500s, a truncated
// body and a 429 and ends up on the new blob, serving the old one throughout.
func TestSyncPollLoopRecovers(t *testing.T) {
	f := newFakeServer(t)
	f.set(buildBlob(t, "A", 64), `"a"`)
	dir := t.TempDir()
	var sleeps []time.Duration
	var mu sync.Mutex
	fast := func(ctx context.Context, d time.Duration) bool {
		mu.Lock()
		sleeps = append(sleeps, d)
		mu.Unlock()
		return sleepCtx(ctx, time.Millisecond)
	}
	s, err := NewSynchronizer(context.Background(), "ds-1", WithBaseURL(f.srv.URL), WithCacheDir(dir),
		WithAPIKey("k"), WithSyncInterval(time.Second), WithTelemetryInterval(0), WithLogger(testLogger(t)),
		func(s *Synchronizer) { s.sleep = fast })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	next := buildBlob(t, "B", 64)
	f.push(statusHandler(500, nil), statusHandler(503, nil), truncatedHandler(next), statusHandler(429, nil))
	f.set(next, `"b"`)
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, err := s.GetCopy([]byte("key_1"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if string(v) == "B_1" {
			break
		}
		if string(v) != "A_1" {
			t.Fatalf("unexpected %q", v)
		}
		if time.Now().After(deadline) {
			t.Fatal("poll loop did not recover")
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	// Sleeps: interval(s), backoff ~1s, ~2s→capped 1s, ~4s→capped 1s, quota 5m, ...
	q := -1
	for i, d := range sleeps {
		if d == QuotaPause {
			q = i
		}
	}
	if q < 3 {
		t.Fatalf("unexpected sleep schedule %v", sleeps)
	}
	for _, d := range sleeps[q-3 : q] {
		if d < 800*time.Millisecond || d > 1200*time.Millisecond {
			t.Fatalf("backoff %v outside 1s±20%% (capped at interval) in %v", d, sleeps)
		}
	}
	t.Logf("sleep schedule: %v", sleeps)
}

// TestSyncServesCacheWhenServerDown: a restarted process with the server down
// serves the previously synced blob; with no cache it fails.
func TestSyncServesCacheWhenServerDown(t *testing.T) {
	f := newFakeServer(t)
	f.set(buildBlob(t, "A", 30), `"a"`)
	dir := t.TempDir()
	s := newTestSync(t, f, dir)
	s.Close()
	f.srv.Close()
	s2, err := NewSynchronizer(context.Background(), "ds-1", WithBaseURL(f.srv.URL), WithCacheDir(dir),
		WithSyncInterval(0), WithTelemetryInterval(0), WithLogger(testLogger(t)))
	if err != nil {
		t.Fatalf("expected cached serve, got %v", err)
	}
	defer s2.Close()
	mustValue(t, s2, "key_5", "A_5")
	if s2.ETag() != `"a"` {
		t.Fatalf("etag not restored: %q", s2.ETag())
	}
	if _, err := NewSynchronizer(context.Background(), "ds-1", WithBaseURL(f.srv.URL), WithCacheDir(t.TempDir()),
		WithSyncInterval(0), WithTelemetryInterval(0), WithLogger(testLogger(t))); err == nil {
		t.Fatal("expected error with no server and no cache")
	}
}

func TestTelemetryEmitter(t *testing.T) {
	f := newFakeServer(t)
	f.set(buildBlob(t, "A", 100), `"a"`)
	s := newTestSync(t, f, t.TempDir(), WithTelemetryInterval(time.Hour), WithAPIKey("tkey"), WithNodeID("node-x"))
	for i := 0; i < 5000; i++ {
		s.Get([]byte(fmt.Sprintf("key_%d", i%100)))
	}
	if err := s.FlushTelemetry(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.telem) == 1 })
	f.mu.Lock()
	body, hdr := f.telem[0], f.telemHdr[0]
	f.mu.Unlock()
	if hdr.Get("X-API-Key") != "tkey" || hdr.Get("Active-Project-ID") != "" || hdr.Get("Content-Type") != "application/json" {
		t.Fatalf("bad telemetry headers: %v", hdr)
	}
	var p otelPayload
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.ResourceSpans) != 1 {
		t.Fatalf("resourceSpans=%d", len(p.ResourceSpans))
	}
	attrs := map[string]string{}
	for _, a := range p.ResourceSpans[0].Resource.Attributes {
		attrs[a.Key] = a.Value.StringValue
	}
	if attrs["attos.dataset_id"] != "ds-1" || attrs["attos.node_id"] != "node-x" || attrs["sdk.version"] != "go-"+Version {
		t.Fatalf("attrs %v", attrs)
	}
	sm := p.ResourceSpans[0].ScopeMetrics[0]
	if sm.Scope.Name != "attos-sdk" {
		t.Fatalf("scope %v", sm.Scope)
	}
	metrics := map[string]int64{}
	for _, m := range sm.Metrics {
		if len(m.Gauge.DataPoints) != 1 || m.Gauge.DataPoints[0].TimeUnixNano == "" {
			t.Fatalf("bad datapoints for %s", m.Name)
		}
		metrics[m.Name] = m.Gauge.DataPoints[0].AsInt
	}
	for _, name := range []string{"attos.lookup.p50_ns", "attos.lookup.p95_ns", "attos.lookup.p99_ns",
		"attos.lookups_count", "attos.memory_bytes", "attos.crc32_errors", "attos.telemetry_dropped"} {
		if v, ok := metrics[name]; !ok || v < 0 {
			t.Fatalf("metric %s missing/negative in %v", name, metrics)
		}
	}
	if metrics["attos.lookups_count"] != 5000 || metrics["attos.memory_bytes"] != s.Map().Size() {
		t.Fatalf("metrics %v", metrics)
	}
	if metrics["attos.lookup.p50_ns"] <= 0 || metrics["attos.lookup.p99_ns"] < metrics["attos.lookup.p50_ns"] {
		t.Fatalf("latency percentiles %v", metrics)
	}
	t.Logf("telemetry body: %s", body)

	// Next window is a delta.
	s.Get([]byte("key_1"))
	s.FlushTelemetry()
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.telem) == 2 })
	if !strings.Contains(string(f.telem[1]), `"name":"attos.lookups_count","gauge":{"dataPoints":[{"asInt":1,`) {
		t.Fatalf("lookups_count not a delta: %s", f.telem[1])
	}
}

func TestTelemetryDisabledByServer(t *testing.T) {
	f := newFakeServer(t)
	f.config = `{"telemetry_enabled":false}`
	f.set(buildBlob(t, "A", 10), `"a"`)
	s := newTestSync(t, f, t.TempDir(), WithTelemetryInterval(time.Hour))
	if s.TelemetryEnabled() {
		t.Fatal("telemetry should be disabled by X-Attos-Config")
	}
	s.FlushTelemetry()
	time.Sleep(50 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.telem) != 0 {
		t.Fatal("telemetry sent while disabled")
	}
}

func TestTelemetryRetryDropAndPause(t *testing.T) {
	f := newFakeServer(t)
	f.set(buildBlob(t, "A", 10), `"a"`)
	var calls atomic.Int32
	f.telemFn = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		calls.Add(1)
		w.WriteHeader(500)
		return true
	}
	s := newTestSync(t, f, t.TempDir(), WithTelemetryInterval(time.Hour))
	s.tel.retryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	s.FlushTelemetry()
	waitFor(t, func() bool { return s.TelemetryStats().DroppedPending == 1 })
	if calls.Load() != 4 {
		t.Fatalf("5xx: %d attempts, want 1+3 retries", calls.Load())
	}
	// 4xx: no retry.
	calls.Store(0)
	f.telemFn = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		calls.Add(1)
		w.WriteHeader(400)
		return true
	}
	s.FlushTelemetry()
	waitFor(t, func() bool { return s.TelemetryStats().DroppedPending == 2 })
	if calls.Load() != 1 {
		t.Fatalf("4xx retried: %d", calls.Load())
	}
	// The dropped count travels in the next report.
	f.telemFn = nil
	s.FlushTelemetry()
	waitFor(t, func() bool { return s.TelemetryStats().Sent == 1 })
	f.mu.Lock()
	last := string(f.telem[len(f.telem)-1])
	f.mu.Unlock()
	if !strings.Contains(last, `"name":"attos.telemetry_dropped","gauge":{"dataPoints":[{"asInt":2,`) {
		t.Fatalf("dropped count not reported: %s", last)
	}
	// 429: drop and pause.
	f.telemFn = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(429)
		return true
	}
	s.FlushTelemetry()
	waitFor(t, func() bool { return s.TelemetryStats().DroppedPending == 1 })
	if until := time.Unix(0, s.tel.pauseUntil.Load()); time.Until(until) < 59*time.Minute {
		t.Fatalf("429 pause not applied: %v", until)
	}
}

func TestTelemetryQueueDropNewest(t *testing.T) {
	f := newFakeServer(t)
	f.set(buildBlob(t, "A", 10), `"a"`)
	block := make(chan struct{})
	f.telemFn = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		<-block
		w.WriteHeader(204)
		return true
	}
	s := newTestSync(t, f, t.TempDir(), WithTelemetryInterval(time.Hour))
	defer close(block)
	for i := 0; i < telemetryQueueSize+20; i++ {
		s.FlushTelemetry()
	}
	st := s.TelemetryStats()
	// One report is in flight (blocked), 64 queued, the rest dropped.
	if st.Queued != telemetryQueueSize || st.DroppedPending < 19 {
		t.Fatalf("queue stats %+v", st)
	}
}

func waitFor(t testing.TB, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
