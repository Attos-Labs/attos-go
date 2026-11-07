package attos

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// redirectEnv is a sync endpoint that answers with a 302 to a second HTTP server (a different host:port, like
// GCS) that serves the bytes.
type redirectEnv struct {
	t     *testing.T
	api   *httptest.Server
	store *httptest.Server

	mu       sync.Mutex
	blob     []byte // current version
	etag     string
	patch    []byte // when set, clients holding fromTag get this as a redirected patch
	fromTag  string
	gzipped  bool
	storeMod func(w http.ResponseWriter, body []byte) // alters what the store serves; nil = intact
	obj      []byte                                   // what the store serves at /obj

	apiReqs   []http.Header
	storeReqs []http.Header
	apiMu     sync.Mutex
}

func newRedirectEnv(t *testing.T, blob []byte) *redirectEnv {
	e := &redirectEnv{t: t, blob: blob, etag: `"1"`}
	e.store = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.apiMu.Lock()
		e.storeReqs = append(e.storeReqs, r.Header.Clone())
		mod, body := e.storeMod, e.obj
		e.apiMu.Unlock()
		if mod != nil {
			mod(w, body)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Write(body)
	}))
	e.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/telemetry/report" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		e.apiMu.Lock()
		e.apiReqs = append(e.apiReqs, r.Header.Clone())
		e.apiMu.Unlock()
		e.mu.Lock()
		blob, etag, patch, from, gz := e.blob, e.etag, e.patch, e.fromTag, e.gzipped
		e.mu.Unlock()
		w.Header().Set("X-Attos-Config", `{"telemetry_enabled":false}`)
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		wantsRedirect := strings.Contains(r.Header.Get(capabilitiesHeader), "redirect") && r.Header.Get(noRedirectHeader) != "1"
		body, isPatch := blob, false
		if patch != nil && r.Header.Get(deltaAcceptHeader) != "" && r.Header.Get("If-None-Match") == from {
			body, isPatch = patch, true
		}
		if isPatch {
			sum := sha256.Sum256(blob)
			w.Header().Set(deltaHeader, "1")
			w.Header().Set(deltaBaseHeader, from)
			w.Header().Set(deltaSHAHeader, hex.EncodeToString(sum[:]))
		}
		served := body
		if gz && !isPatch && r.Header.Get("Accept-Encoding") == "gzip" {
			var b bytes.Buffer
			zw := gzip.NewWriter(&b)
			zw.Write(body)
			zw.Close()
			served = b.Bytes()
			if wantsRedirect {
				w.Header().Set(contentEncodingHdr, "gzip")
			} else {
				w.Header().Set("Content-Encoding", "gzip")
			}
		}
		if !wantsRedirect {
			w.Header().Set("Content-Length", fmt.Sprint(len(served)))
			w.Write(served)
			return
		}
		e.apiMu.Lock()
		e.obj = served
		e.apiMu.Unlock()
		w.Header().Set(contentLengthHdr, fmt.Sprint(len(served)))
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Location", e.store.URL+"/obj?X-Goog-Signature=abc")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(e.api.Close)
	t.Cleanup(e.store.Close)
	return e
}

func (e *redirectEnv) sync(t *testing.T, opts ...Option) *Synchronizer {
	all := append([]Option{WithBaseURL(e.api.URL), WithCacheDir(t.TempDir()), WithAPIKey("secret-api-key"), WithNodeID("node-1"),
		WithSyncInterval(0), WithTelemetryInterval(0), WithLogger(testLogger(t)), withJitterSource(func() float64 { return 0 })}, opts...)
	s, err := NewSynchronizer(context.Background(), "ds-1", all...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func (e *redirectEnv) publish(blob []byte, patchFrom []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fromTag = e.etag
	if patchFrom != nil {
		e.patch = makePatch(patchFrom, blob)
	}
	e.blob = blob
	e.etag = fmt.Sprintf(`"%d"`, len(e.apiReqs)+2)
}

func (e *redirectEnv) lastAPI() http.Header {
	e.apiMu.Lock()
	defer e.apiMu.Unlock()
	return e.apiReqs[len(e.apiReqs)-1]
}
func (e *redirectEnv) nAPI() int   { e.apiMu.Lock(); defer e.apiMu.Unlock(); return len(e.apiReqs) }
func (e *redirectEnv) nStore() int { e.apiMu.Lock(); defer e.apiMu.Unlock(); return len(e.storeReqs) }

func TestRedirectDownloadsWithoutCredentials(t *testing.T) {
	e := newRedirectEnv(t, buildBlob(t, "a", 300))
	s := e.sync(t)
	wantValue(t, s, "key_5", "a_5")
	if r, fb := s.RedirectStats(); r != 1 || fb != 0 {
		t.Fatalf("redirected=%d fallbacks=%d", r, fb)
	}
	if !strings.Contains(e.apiReqs[0].Get(capabilitiesHeader), "redirect") {
		t.Fatalf("capability not sent: %v", e.apiReqs[0])
	}
	if e.nStore() != 1 {
		t.Fatalf("store requests %d", e.nStore())
	}
	for k := range e.storeReqs[0] {
		switch strings.ToLower(k) {
		case "x-api-key", "authorization", "cookie", "x-attos-node-id", "x-attos-capabilities", "if-none-match":
			t.Fatalf("credential/identity header %s sent to the download host", k)
		}
	}
	if s.ETag() != `"1"` {
		t.Fatalf("etag %q", s.ETag())
	}
}

func TestRedirectFailuresFallBackToStreaming(t *testing.T) {
	blob := buildBlob(t, "a", 300)
	cases := map[string]func(w http.ResponseWriter, body []byte){
		"expired URL": func(w http.ResponseWriter, body []byte) { http.Error(w, "ExpiredToken", http.StatusForbidden) },
		"corrupted body": func(w http.ResponseWriter, body []byte) {
			c := append([]byte(nil), body...)
			c[len(c)/2] ^= 0xff
			w.Header().Set("Content-Length", fmt.Sprint(len(c)))
			w.Write(c)
		},
		"truncated body": func(w http.ResponseWriter, body []byte) {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.Write(body[:len(body)/2])
		},
		"oversized chunked body": func(w http.ResponseWriter, body []byte) { // no Content-Length: the stream is cut at declared+1
			w.(http.Flusher).Flush()
			w.Write(body)
			w.Write(bytes.Repeat([]byte{1}, 1<<16))
		},
		"wrong length": func(w http.ResponseWriter, body []byte) {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)-1))
			w.Write(body[:len(body)-1])
		},
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			e := newRedirectEnv(t, blob)
			e.storeMod = mod
			s := e.sync(t)
			wantValue(t, s, "key_9", "a_9")
			if r, fb := s.RedirectStats(); r != 0 || fb != 1 {
				t.Fatalf("redirected=%d fallbacks=%d", r, fb)
			}
			if e.nAPI() != 2 {
				t.Fatalf("api requests %d, want 2 (redirect, then the fallback)", e.nAPI())
			}
			h := e.lastAPI()
			if h.Get(noRedirectHeader) != "1" || strings.Contains(h.Get(capabilitiesHeader), "redirect") {
				t.Fatalf("fallback request headers: %v", h)
			}
		})
	}
}

func TestRedirectGzipCopy(t *testing.T) {
	e := newRedirectEnv(t, buildBlob(t, "a", 300))
	e.gzipped = true
	s := e.sync(t)
	wantValue(t, s, "key_1", "a_1")
	if r, fb := s.RedirectStats(); r != 1 || fb != 0 {
		t.Fatalf("redirected=%d fallbacks=%d", r, fb)
	}
}

func TestRedirectOffAndLegacyServer(t *testing.T) {
	e := newRedirectEnv(t, buildBlob(t, "a", 100))
	s := e.sync(t, WithRedirect(false))
	wantValue(t, s, "key_1", "a_1")
	h := e.apiReqs[0]
	if strings.Contains(h.Get(capabilitiesHeader), "redirect") || h.Get(noRedirectHeader) != "" || e.nStore() != 0 {
		t.Fatalf("WithRedirect(false) must stream without any redirect header: %v", h)
	}
	// a server that never redirects (an older backend) is served as before
	e2 := newRedirectEnv(t, buildBlob(t, "b", 100))
	e2.api.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/telemetry/report" {
			w.WriteHeader(204)
			return
		}
		w.Header().Set("ETag", `"x"`)
		w.Write(e2.blob)
	})
	s2 := e2.sync(t)
	wantValue(t, s2, "key_2", "b_2")
	if r, fb := s2.RedirectStats(); r != 0 || fb != 0 {
		t.Fatalf("%d %d", r, fb)
	}
}

// A plain redirect (no X-Attos-Content-Length) is followed, but the API key and node ID never travel to
// another host.
func TestOrdinaryRedirectDoesNotForwardCredentials(t *testing.T) {
	blob := buildBlob(t, "a", 100)
	var other http.Header
	var mu sync.Mutex
	far := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		other = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("ETag", `"far"`)
		w.Write(blob)
	}))
	defer far.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/telemetry/report" {
			w.WriteHeader(204)
			return
		}
		http.Redirect(w, r, far.URL+"/blob", http.StatusFound)
	}))
	defer api.Close()
	s, err := NewSynchronizer(context.Background(), "ds-1", WithBaseURL(api.URL), WithCacheDir(t.TempDir()), WithAPIKey("secret-api-key"),
		WithNodeID("node-1"), WithSyncInterval(0), WithTelemetryInterval(0), WithLogger(testLogger(t)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	wantValue(t, s, "key_1", "a_1")
	mu.Lock()
	defer mu.Unlock()
	for _, k := range []string{"X-Api-Key", "Authorization", "X-Attos-Node-Id"} {
		if other.Get(k) != "" {
			t.Fatalf("%s forwarded to another host", k)
		}
	}
}

func TestRedirectedPatchAndFallback(t *testing.T) {
	a, b := buildBlob(t, "a", 400), buildBlob(t, "b", 400)
	e := newRedirectEnv(t, a)
	s := e.sync(t)
	wantValue(t, s, "key_7", "a_7")
	e.publish(b, a)
	e.mu.Lock()
	e.mu.Unlock()
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantValue(t, s, "key_7", "b_7")
	if ap, fb, _ := s.DeltaStats(); ap != 1 || fb != 0 {
		t.Fatalf("delta applied=%d fallbacks=%d", ap, fb)
	}
	if r, _ := s.RedirectStats(); r != 2 {
		t.Fatalf("redirected %d, want 2 (full blob, then patch)", r)
	}

	// a corrupted redirected patch: streaming is tried next, and it also gets the patch (the server prefers it
	// for this client); that patch is intact, so the sync completes
	c := buildBlob(t, "c", 400)
	e.publish(c, b)
	e.storeMod = func(w http.ResponseWriter, body []byte) {
		x := append([]byte(nil), body...)
		x[len(x)/2] ^= 1
		w.Header().Set("Content-Length", fmt.Sprint(len(x)))
		w.Write(x)
	}
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("sync must survive a corrupted redirected patch: %v", err)
	}
	wantValue(t, s, "key_7", "c_7")
	if _, fb := s.RedirectStats(); fb != 1 {
		t.Fatalf("redirect fallbacks %d", fb)
	}
}

func TestRedirectedHotSwapUnderReads(t *testing.T) {
	const n = 1500
	a, b := buildBlob(t, "a", n), buildBlob(t, "b", n)
	e := newRedirectEnv(t, a)
	s := e.sync(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var bad, reads atomic.Uint64
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := g; ; i += 8 {
				select {
				case <-stop:
					return
				default:
				}
				k := i % n
				v, err := s.Get([]byte(fmt.Sprintf("key_%d", k)))
				reads.Add(1)
				if err != nil || (string(v) != fmt.Sprintf("a_%d", k) && string(v) != fmt.Sprintf("b_%d", k)) {
					bad.Add(1)
				}
			}
		}(g)
	}
	time.Sleep(20 * time.Millisecond)
	e.publish(b, a)
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	close(stop)
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d bad reads of %d", bad.Load(), reads.Load())
	}
	wantValue(t, s, "key_3", "b_3")
}

func TestPollJitter(t *testing.T) {
	const base = 30 * time.Second
	for _, c := range []struct {
		r    float64
		want time.Duration
	}{{0, base}, {0.5, base + base/4}, {0.999999, base + base/2 - time.Microsecond}} {
		r := c.r
		s := &Synchronizer{syncInterval: base, loggedStatus: map[int]bool{}, logger: testLogger(t), rnd: func() float64 { return r }}
		d := s.nextDelay(nil)
		if d < c.want-time.Millisecond || d > c.want+time.Millisecond {
			t.Fatalf("r=%v: delay %v, want about %v", c.r, d, c.want)
		}
		if d < base || d >= base+base/2 {
			t.Fatalf("delay %v outside [30s, 45s)", d)
		}
		// a rejected request (4xx) is retried on the same jittered schedule
		if d4 := s.nextDelay(&StatusError{Code: 401}); d4 != d {
			t.Fatalf("4xx delay %v, want %v", d4, d)
		}
		// a quota pause is never shorter than asked, at most 20% longer
		if dq := s.nextDelay(&StatusError{Code: 429, RetryAfter: 90 * time.Second}); dq < 90*time.Second || dq > 108*time.Second {
			t.Fatalf("quota pause %v", dq)
		}
		// transient back-off keeps its +-20% jitter
		s.failures = 3
		if db := s.nextDelay(fmt.Errorf("boom")); db < 6400*time.Millisecond || db > 9600*time.Millisecond {
			t.Fatalf("backoff %v", db)
		}
	}
	// the default source spreads delays and stays in range
	s := &Synchronizer{syncInterval: base, loggedStatus: map[int]bool{}, logger: testLogger(t)}
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		d := s.nextDelay(nil)
		if d < base || d >= base+base/2 {
			t.Fatalf("delay %v out of range", d)
		}
		seen[d/time.Second] = true
	}
	if len(seen) < 10 {
		t.Fatalf("only %d distinct whole-second delays in 200 draws", len(seen))
	}
	if z := (&Synchronizer{syncInterval: 0}).jitterInterval(); z != 0 {
		t.Fatalf("polling off must stay off: %v", z)
	}
}
