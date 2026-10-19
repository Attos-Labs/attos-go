package attos

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func gzipBytes(t testing.TB, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

// gzipHandler serves blob gzip-compressed with etag, like the service does
// for clients that send Accept-Encoding: gzip.
func gzipHandler(t testing.TB, wire []byte, etag string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			t.Errorf("sync request without Accept-Encoding: gzip: %v", r.Header)
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Vary", "Accept-Encoding")
		w.Header().Set("Content-Length", fmt.Sprint(len(wire)))
		w.Write(wire)
	}
}

// TestSyncGzip: a gzip response is decompressed, validated as a v1 blob and
// swapped in; the persisted file is the raw v1 blob.
func TestSyncGzip(t *testing.T) {
	f := newFakeServer(t)
	blobA := buildBlob(t, "A", 200)
	f.set(blobA, `"a"`)
	f.push(gzipHandler(t, gzipBytes(t, blobA), `"a-gzip"`))
	s := newTestSync(t, f, t.TempDir())
	mustValue(t, s, "key_9", "A_9")
	if s.ETag() != `"a-gzip"` {
		t.Fatalf("etag %q", s.ETag())
	}

	// A new version arrives compressed too.
	blobB := buildBlob(t, "B", 200)
	f.push(gzipHandler(t, gzipBytes(t, blobB), `"b-gzip"`))
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustValue(t, s, "key_9", "B_9")
}

// TestSyncGzipFaults: a truncated or corrupted gzip stream, or gzip around a
// blob with a bad CRC, is rejected and the old map keeps serving.
func TestSyncGzipFaults(t *testing.T) {
	f := newFakeServer(t)
	blobA := buildBlob(t, "A", 200)
	f.set(blobA, `"a"`)
	s := newTestSync(t, f, t.TempDir())
	mustValue(t, s, "key_9", "A_9")

	blobB := buildBlob(t, "B", 200)
	wire := gzipBytes(t, blobB)
	badCRC := append([]byte(nil), blobB...)
	badCRC[len(badCRC)-1] ^= 0xff
	corrupt := append([]byte(nil), wire...)
	corrupt[len(corrupt)/2] ^= 0xff

	for name, h := range map[string]func(http.ResponseWriter, *http.Request){
		"truncated gzip": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("ETag", `"t"`)
			w.Write(wire[:len(wire)/2])
		},
		"corrupted gzip": gzipHandler(t, corrupt, `"c"`),
		"bad blob CRC":   gzipHandler(t, gzipBytes(t, badCRC), `"x"`),
		"unknown coding": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Encoding", "br")
			w.Write(blobB)
		},
	} {
		f.push(h)
		if err := s.Sync(context.Background()); err == nil {
			t.Fatalf("%s: accepted", name)
		}
		mustValue(t, s, "key_9", "A_9")
		if s.ETag() != `"a"` {
			t.Fatalf("%s: etag changed to %q", name, s.ETag())
		}
	}
	if !errors.Is(func() error {
		f.push(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			w.Write(wire[:5])
		})
		return s.Sync(context.Background())
	}(), ErrTruncatedDownload) {
		t.Fatal("a cut gzip header must be ErrTruncatedDownload")
	}
}

// TestSyncCompressionOff: WithCompression(false) asks for the raw blob.
func TestSyncCompressionOff(t *testing.T) {
	f := newFakeServer(t)
	f.set(buildBlob(t, "A", 10), `"a"`)
	s := newTestSync(t, f, t.TempDir(), WithCompression(false))
	mustValue(t, s, "key_1", "A_1")
	if ae := f.syncReqs[0].Header.Get("Accept-Encoding"); strings.Contains(ae, "gzip") {
		t.Fatalf("Accept-Encoding %q with compression off", ae)
	}
}
