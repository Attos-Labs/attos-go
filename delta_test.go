package attos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// chainBase is a v2 file whose key_i value is val_i; chainVariant(base, tag, n) is the same main blob with an overflow
// that sets key_0..key_n-1 to tag_i (a variant of the chain).
func chainBase(t testing.TB, n int) []byte { return buildMode(t, n, CompileOptions{}) }

func chainVariant(t testing.TB, base []byte, tag string, n int) []byte {
	var items []ovItem
	for i := 0; i < n; i++ {
		items = append(items, ovItem{key: []byte(fmt.Sprintf("key_%d", i)), val: []byte(fmt.Sprintf("%s_%d", tag, i))})
	}
	return withOverflow(t, base, items)
}

func apply(old, patch []byte) ([]byte, error) {
	var out bytes.Buffer
	_, err := applyDelta(&out, bytes.NewReader(old), int64(len(old)), bytes.NewReader(patch))
	return out.Bytes(), err
}

func TestApplyDeltaRejectsDamage(t *testing.T) {
	base := chainBase(t, 300)
	nw := chainVariant(t, base, "b", 20)
	p := makeTailPatch(base, nw)
	if got, err := apply(base, p); err != nil || !bytes.Equal(got, nw) {
		t.Fatalf("valid patch: %v", err)
	}
	for i := 0; i < len(p); i += 5 {
		c := append([]byte(nil), p...)
		c[i] ^= 0x20
		if _, err := apply(base, c); err == nil {
			t.Fatalf("byte flip at %d accepted", i)
		}
	}
	for _, n := range []int{0, 30, len(p) / 2, len(p) - 1} {
		if _, err := apply(base, p[:n]); err == nil {
			t.Fatalf("truncation to %d accepted", n)
		}
	}
	// A different main blob rebuilds a file whose SHA-256 is not the patch's: refused.
	if _, err := apply(buildMode(t, 305, CompileOptions{}), p); !errors.Is(err, errDeltaCorrupt) {
		t.Fatalf("unrelated base: %v", err)
	}
	// Any file of the chain is a base: the previous variant takes the same patch.
	prev := chainVariant(t, base, "a", 5)
	if got, err := apply(prev, p); err != nil || !bytes.Equal(got, nw) {
		t.Fatalf("patch on a previous variant: %v", err)
	}
	// Version 1 (copy/add) patches no longer exist.
	v1 := append([]byte(nil), p...)
	binary.BigEndian.PutUint32(v1[4:], 1)
	if _, err := apply(base, v1); err == nil {
		t.Fatal("a version 1 patch was accepted")
	}
	_ = crc32.ChecksumIEEE
}

// deltaServer serves full blobs, and the tail patch to clients that send the patch capability and hold one of okTags.
type deltaServer struct {
	*fakeServer
	patch   atomic.Pointer[[]byte]
	okTags  map[string]bool
	mutate  func(w http.ResponseWriter, body []byte) []byte // damages a patch response; nil = intact
	deltaRq atomic.Int32
	fullRq  atomic.Int32
	sawAcc  atomic.Int32
	gen     int
	main    []byte
}

func newDeltaServer(t *testing.T, blob []byte) *deltaServer {
	d := &deltaServer{fakeServer: newFakeServer(t), okTags: map[string]bool{`"1"`: true}, main: blob}
	d.set(blob, `"1"`)
	d.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantsPatch := strings.Contains(r.Header.Get(capabilitiesHeader), "patch")
		if wantsPatch {
			d.sawAcc.Add(1)
		}
		p := d.patch.Load()
		if p != nil && wantsPatch && d.okTags[r.Header.Get("If-None-Match")] {
			d.deltaRq.Add(1)
			body := *p
			if d.mutate != nil {
				body = d.mutate(w, body)
			}
			d.mu.Lock()
			etag := d.etag
			target := d.blob
			d.mu.Unlock()
			sum := sha256.Sum256(target)
			w.Header().Set(deltaHeader, "1")
			w.Header().Set(deltaSHAHeader, hex.EncodeToString(sum[:]))
			w.Header().Set("ETag", etag)
			w.Header().Set("X-Attos-Config", `{"telemetry_enabled":true}`)
			w.Write(body)
			return
		}
		if r.URL.Path != "/api/v1/telemetry/report" {
			d.fullRq.Add(1)
		}
		d.handle(w, r)
	})
	return d
}

// publish makes nw (a variant of the chain's main blob) the live file. Every earlier tag of the chain gets the one
// patch built from the main blob, however many versions behind it is.
func (d *deltaServer) publish(nw []byte) {
	d.mu.Lock()
	d.okTags[d.etag] = true
	gen := d.gen + 1
	d.gen = gen
	d.mu.Unlock()
	p := makeTailPatch(d.main, nw)
	d.patch.Store(&p)
	d.set(nw, fmt.Sprintf(`"%d"`, gen+1))
}

func syncer(t *testing.T, d *deltaServer, opts ...Option) *Synchronizer {
	t.Helper()
	return newTestSync(t, d.fakeServer, t.TempDir(), opts...)
}

func wantValue(t *testing.T, s *Synchronizer, key, want string) {
	t.Helper()
	got, err := s.GetString(key)
	if err != nil || got != want {
		t.Fatalf("Get(%s) = %q, %v; want %q", key, got, err, want)
	}
}

func TestDeltaSyncAppliesPatch(t *testing.T) {
	base := chainBase(t, 500)
	d := newDeltaServer(t, base)
	s := syncer(t, d)
	wantValue(t, s, "key_7", "val_7")
	d.publish(chainVariant(t, base, "b", 50))
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantValue(t, s, "key_7", "b_7")
	if a, fb, bytes := s.DeltaStats(); a != 1 || fb != 0 || bytes == 0 {
		t.Fatalf("stats applied=%d fallbacks=%d bytes=%d", a, fb, bytes)
	}
	if d.deltaRq.Load() != 1 || d.fullRq.Load() != 1 {
		t.Fatalf("delta requests %d, full requests %d (want 1 and 1)", d.deltaRq.Load(), d.fullRq.Load())
	}
	if err := s.Sync(context.Background()); err != nil { // up to date: 304, no patch
		t.Fatal(err)
	}
	if s.ETag() != `"2"` {
		t.Fatalf("etag %q", s.ETag())
	}
}

// A node several versions behind takes the one patch of the live version, then a node one behind takes it too.
func TestDeltaSyncSkipsVersions(t *testing.T) {
	base := chainBase(t, 500)
	d := newDeltaServer(t, base)
	slow := syncer(t, d)
	d.publish(chainVariant(t, base, "b", 30))
	fast := syncer(t, d) // joins at version 2 (full download)
	d.publish(chainVariant(t, base, "c", 40))
	d.publish(chainVariant(t, base, "d", 50))
	for name, s := range map[string]*Synchronizer{"slow (3 behind)": slow, "fast (2 behind)": fast} {
		if err := s.Sync(context.Background()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		wantValue(t, s, "key_9", "d_9")
		if a, fb, _ := s.DeltaStats(); a != 1 || fb != 0 {
			t.Fatalf("%s: applied=%d fallbacks=%d", name, a, fb)
		}
	}
}

func TestDeltaFallsBackToFullBlob(t *testing.T) {
	cases := map[string]func(d *deltaServer){
		"flipped byte": func(d *deltaServer) {
			d.mutate = func(w http.ResponseWriter, b []byte) []byte {
				c := append([]byte(nil), b...)
				c[len(c)/2] ^= 1
				return c
			}
		},
		"truncated": func(d *deltaServer) {
			d.mutate = func(w http.ResponseWriter, b []byte) []byte { return b[:len(b)/2] }
		},
		"garbage": func(d *deltaServer) {
			d.mutate = func(w http.ResponseWriter, b []byte) []byte { return bytes.Repeat([]byte{7}, len(b)) }
		},
		"patch for another chain": func(d *deltaServer) {
			other := buildMode(t, 520, CompileOptions{})
			p := makeTailPatch(other, chainVariant(t, other, "z", 10))
			d.patch.Store(&p)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			base := chainBase(t, 500)
			d := newDeltaServer(t, base)
			s := syncer(t, d)
			d.publish(chainVariant(t, base, "b", 50))
			setup(d)
			if err := s.Sync(context.Background()); err != nil {
				t.Fatalf("sync must succeed through the fallback: %v", err)
			}
			wantValue(t, s, "key_9", "b_9")
			if a, fb, _ := s.DeltaStats(); a != 0 || fb != 1 {
				t.Fatalf("applied=%d fallbacks=%d", a, fb)
			}
			if d.fullRq.Load() != 2 { // initial download + the fallback
				t.Fatalf("full downloads: %d", d.fullRq.Load())
			}
			if s.ETag() != `"2"` {
				t.Fatalf("etag %q", s.ETag())
			}
		})
	}
}

func TestDeltaTargetShaHeaderMismatchFallsBack(t *testing.T) {
	base := chainBase(t, 400)
	d := newDeltaServer(t, base)
	s := syncer(t, d)
	d.publish(chainVariant(t, base, "b", 30))
	// A patch that rebuilds a different (valid) file than the server says it serves.
	p := makeTailPatch(base, chainVariant(t, base, "x", 30))
	d.patch.Store(&p)
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantValue(t, s, "key_3", "b_3")
	if _, fb, _ := s.DeltaStats(); fb != 1 {
		t.Fatalf("fallbacks=%d", fb)
	}
}

func TestWithDeltaOffNeverAsks(t *testing.T) {
	base := chainBase(t, 200)
	d := newDeltaServer(t, base)
	s := syncer(t, d, WithDelta(false))
	d.publish(chainVariant(t, base, "b", 20))
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantValue(t, s, "key_1", "b_1")
	if d.sawAcc.Load() != 0 || d.deltaRq.Load() != 0 {
		t.Fatal("patch capability sent although disabled")
	}
}

// Readers hammer the map while a patch is applied and swapped in: every read must be a whole old or new
// value (never an error, never a torn value), and the swap must complete.
func TestDeltaHotSwapUnderConcurrentReads(t *testing.T) {
	const n = 2000
	base := chainBase(t, n)
	d := newDeltaServer(t, base)
	s := syncer(t, d)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var reads, bad atomic.Uint64
	var sawNew atomic.Bool
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
				switch {
				case err != nil:
					bad.Add(1)
				case string(v) == fmt.Sprintf("val_%d", k):
				case k < 100 && string(v) == fmt.Sprintf("b_%d", k):
					sawNew.Store(true)
				default:
					bad.Add(1)
				}
			}
		}(g)
	}
	time.Sleep(20 * time.Millisecond)
	d.publish(chainVariant(t, base, "b", 100))
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d bad reads out of %d", bad.Load(), reads.Load())
	}
	if !sawNew.Load() {
		t.Fatal("readers never observed the patched map")
	}
	if a, _, _ := s.DeltaStats(); a != 1 {
		t.Fatalf("patch not used (applied=%d)", a)
	}
	t.Logf("%d reads across the swap, 0 bad", reads.Load())
}
