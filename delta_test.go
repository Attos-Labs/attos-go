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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// makePatch is a test-only encoder: common prefix and suffix are COPYs, the middle is one ADD. It is
// independent of the server's diff, so these tests check the wire format, not a shared implementation.
func makePatch(old, nw []byte) []byte {
	pre := 0
	for pre < len(old)-4 && pre < len(nw)-4 && old[pre] == nw[pre] {
		pre++
	}
	suf := 0
	for suf < len(old)-pre && suf < len(nw)-pre && old[len(old)-1-suf] == nw[len(nw)-1-suf] {
		suf++
	}
	var b bytes.Buffer
	b.WriteString("ATDL")
	var u [binary.MaxVarintLen64]byte
	binary.Write(&b, binary.BigEndian, uint32(1))
	binary.Write(&b, binary.BigEndian, uint64(len(old)))
	b.Write(old[len(old)-4:])
	binary.Write(&b, binary.BigEndian, uint64(len(nw)))
	sum := sha256.Sum256(nw)
	b.Write(sum[:])
	uv := func(v uint64) { b.Write(u[:binary.PutUvarint(u[:], v)]) }
	if pre > 0 {
		b.WriteByte(1)
		uv(0)
		uv(uint64(pre))
	}
	if mid := nw[pre : len(nw)-suf]; len(mid) > 0 {
		b.WriteByte(2)
		uv(uint64(len(mid)))
		b.Write(mid)
	}
	if suf > 0 {
		b.WriteByte(1)
		uv(uint64(len(old) - suf))
		uv(uint64(suf))
	}
	b.WriteByte(0)
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(b.Bytes()))
	return b.Bytes()
}

func apply(old, patch []byte) ([]byte, error) {
	var out bytes.Buffer
	_, err := applyDelta(&out, bytes.NewReader(old), int64(len(old)), bytes.NewReader(patch))
	return out.Bytes(), err
}

// The same golden patch is pinned in attos-cloud/backend/internal/delta (TestGoldenPatch).
func TestDeltaGoldenPatch(t *testing.T) {
	const patchHex = "4154444c000000010000000000000190e2ad7cbb000000000000019b2f513f00e060e0ef3cbcf14e1c0a846d49b7e118a6585039aacde5f8e0321e240100c801020b68656c6c6f2064656c746101c801c80100d172ddf2"
	patch, _ := hex.DecodeString(patchHex)
	sum := sha256.Sum256(patch)
	if got := hex.EncodeToString(sum[:]); got != "76a3e6585d9f95f9381eece4c1650f8b9d0faa187c52253ece0d6a1020e22e61" {
		t.Fatalf("golden patch bytes changed: %s", got)
	}
	old := make([]byte, 400)
	for i := range old {
		old[i] = byte(i*7 + i/13)
	}
	crc := crc32.ChecksumIEEE(old[:396])
	binary.BigEndian.PutUint32(old[396:], crc)
	want := append(append(append([]byte(nil), old[:200]...), "hello delta"...), old[200:]...)
	got, err := apply(old, patch)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("golden patch did not rebuild the target: %v", err)
	}
}

func TestApplyDeltaRejectsDamage(t *testing.T) {
	old, nw := buildBlob(t, "a", 300), buildBlob(t, "b", 300)
	p := makePatch(old, nw)
	if got, err := apply(old, p); err != nil || !bytes.Equal(got, nw) {
		t.Fatalf("valid patch: %v", err)
	}
	for i := 0; i < len(p); i += 5 {
		c := append([]byte(nil), p...)
		c[i] ^= 0x20
		if _, err := apply(old, c); err == nil {
			t.Fatalf("byte flip at %d accepted", i)
		}
	}
	for _, n := range []int{0, 30, len(p) / 2, len(p) - 1} {
		if _, err := apply(old, p[:n]); err == nil {
			t.Fatalf("truncation to %d accepted", n)
		}
	}
	if _, err := apply(buildBlob(t, "c", 300), p); !errors.Is(err, errDeltaBase) {
		t.Fatalf("wrong base: %v", err)
	}
}

// deltaServer serves full blobs, and patches to clients that hold fromTag and ask for them.
type deltaServer struct {
	*fakeServer
	patch   atomic.Pointer[[]byte]
	fromTag string
	mutate  func(w http.ResponseWriter, body []byte) []byte // damages a patch response; nil = intact
	deltaRq atomic.Int32
	fullRq  atomic.Int32
	sawAcc  atomic.Int32
}

func newDeltaServer(t *testing.T, blob []byte) *deltaServer {
	d := &deltaServer{fakeServer: newFakeServer(t)}
	d.set(blob, `"1"`)
	d.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(deltaAcceptHeader) == "1" {
			d.sawAcc.Add(1)
		}
		p := d.patch.Load()
		if p != nil && r.Header.Get(deltaAcceptHeader) == "1" && r.Header.Get("If-None-Match") == d.fromTag && d.fromTag != "" {
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
			w.Header().Set(deltaBaseHeader, d.fromTag)
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

func (d *deltaServer) publish(nw []byte) {
	d.mu.Lock()
	old := d.blob
	d.fromTag = d.etag
	d.mu.Unlock()
	p := makePatch(old, nw)
	d.patch.Store(&p)
	d.set(nw, `"2"`)
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
	d := newDeltaServer(t, buildBlob(t, "a", 500))
	s := syncer(t, d)
	wantValue(t, s, "key_7", "a_7")
	d.publish(buildBlob(t, "b", 500))
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
		"patch for another base": func(d *deltaServer) { d.fromTag = `"1"`; d.patchFor(buildBlob(t, "z", 500), d.blobNow()) },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			d := newDeltaServer(t, buildBlob(t, "a", 500))
			s := syncer(t, d)
			d.publish(buildBlob(t, "b", 500))
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

func (d *deltaServer) blobNow() []byte { d.mu.Lock(); defer d.mu.Unlock(); return d.blob }
func (d *deltaServer) patchFor(old, nw []byte) {
	p := makePatch(old, nw)
	d.patch.Store(&p)
}

func TestDeltaTargetShaHeaderMismatchFallsBack(t *testing.T) {
	d := newDeltaServer(t, buildBlob(t, "a", 400))
	s := syncer(t, d)
	d.publish(buildBlob(t, "b", 400))
	// A patch that rebuilds a different (valid) blob than the server says it serves.
	other := buildBlob(t, "x", 400)
	p := makePatch(d.fakeServer.blobFor(`"1"`, d), other)
	d.patch.Store(&p)
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantValue(t, s, "key_3", "b_3")
	if _, fb, _ := s.DeltaStats(); fb != 1 {
		t.Fatalf("fallbacks=%d", fb)
	}
}

func (f *fakeServer) blobFor(_ string, d *deltaServer) []byte {
	// The base blob is what the node holds: rebuild it the way the test did.
	return buildBlob(f.t, "a", 400)
}

func TestWithDeltaOffNeverAsks(t *testing.T) {
	d := newDeltaServer(t, buildBlob(t, "a", 200))
	s := syncer(t, d, WithDelta(false))
	d.publish(buildBlob(t, "b", 200))
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantValue(t, s, "key_1", "b_1")
	if d.sawAcc.Load() != 0 || d.deltaRq.Load() != 0 {
		t.Fatal("delta header sent although disabled")
	}
}

// Readers hammer the map while a patch is applied and swapped in: every read must be a whole old or new
// value (never an error, never a torn value), and the swap must complete.
func TestDeltaHotSwapUnderConcurrentReads(t *testing.T) {
	const n = 2000
	d := newDeltaServer(t, buildBlob(t, "a", n))
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
				case string(v) == fmt.Sprintf("a_%d", k):
				case string(v) == fmt.Sprintf("b_%d", k):
					sawNew.Store(true)
				default:
					bad.Add(1)
				}
			}
		}(g)
	}
	time.Sleep(20 * time.Millisecond)
	d.publish(buildBlob(t, "b", n))
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
