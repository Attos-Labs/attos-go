package attos

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

type signingServer struct {
	srv    *httptest.Server
	blob   atomic.Pointer[[]byte]
	sigFn  atomic.Pointer[func(digest []byte) string]
	hits   atomic.Int64
	etagNo atomic.Int64
}

func newSigningServer(t *testing.T) *signingServer {
	s := &signingServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/datasets/sync/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.hits.Add(1)
		blob := *s.blob.Load()
		sum := sha256.Sum256(blob)
		if fn := s.sigFn.Load(); fn != nil {
			if h := (*fn)(sum[:]); h != "" {
				w.Header().Set(SignatureHeader, h)
			}
		}
		w.Header().Set("ETag", `"`+string(rune('a'+s.etagNo.Load()))+`"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
		w.Write(blob)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func readBlob(t *testing.T, path string) *[]byte {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return &b
}

func TestSignedArtifacts(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	otherPub, otherPriv, _ := ed25519.GenerateKey(nil)
	_ = otherPub
	srv := newSigningServer(t)
	srv.blob.Store(readBlob(t, kvBlob(t, 50)))
	sign := func(priv ed25519.PrivateKey, kid, stash string) *func([]byte) string {
		f := func(d []byte) string { return SignArtifact(priv, kid, stash, d) }
		return &f
	}
	open := func(dir string, keys map[string]ed25519.PublicKey) (*Synchronizer, error) {
		return NewSynchronizer(context.Background(), "ds-1", WithBaseURL(srv.srv.URL), WithCacheDir(dir),
			WithSyncInterval(0), WithTelemetryDisabled(), WithSignatureKeys(keys))
	}
	keys := map[string]ed25519.PublicKey{"k1": pub}

	t.Run("valid signature installs", func(t *testing.T) {
		srv.sigFn.Store(sign(priv, "k1", "ds-1"))
		s, err := open(t.TempDir(), keys)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if v, err := s.Get([]byte("k7")); err != nil || string(v) != "v7" {
			t.Fatalf("%q %v", v, err)
		}
		if s.sigErrors.Load() != 0 {
			t.Fatal("unexpected signature error")
		}
	})

	reject := map[string]*func([]byte) string{
		"missing signature": nil,
		"wrong key":         sign(otherPriv, "k1", "ds-1"),
		"unknown key id":    sign(priv, "nope", "ds-1"),
		"other stash":       sign(priv, "k1", "ds-2"),
	}
	for name, fn := range reject {
		t.Run("rejects "+name, func(t *testing.T) {
			srv.sigFn.Store(fn)
			s, err := open(t.TempDir(), keys)
			if err == nil {
				s.Close()
				t.Fatal("expected initial sync to fail")
			}
			if !errors.Is(err, ErrSignatureMissing) && !errors.Is(err, ErrSignatureInvalid) {
				t.Fatalf("wrong error: %v", err)
			}
		})
	}

	t.Run("bad update keeps serving the verified file and counts the error", func(t *testing.T) {
		srv.sigFn.Store(sign(priv, "k1", "ds-1"))
		s, err := open(t.TempDir(), keys)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		srv.blob.Store(readBlob(t, kvBlob(t, 60)))
		srv.etagNo.Add(1)
		srv.sigFn.Store(sign(otherPriv, "k1", "ds-1"))
		if err := s.Sync(context.Background()); !errors.Is(err, ErrSignatureInvalid) {
			t.Fatalf("want ErrSignatureInvalid, got %v", err)
		}
		if s.sigErrors.Load() != 1 {
			t.Fatalf("sigErrors=%d", s.sigErrors.Load())
		}
		if _, err := s.Get([]byte("k55")); err == nil {
			t.Fatal("new, unsigned file must not be served")
		}
		if _, err := s.Get([]byte("k7")); err != nil {
			t.Fatal("old file must keep serving:", err)
		}
		var b strings.Builder
		s.WritePrometheus(&b)
		if !strings.Contains(b.String(), "attos_signature_errors_total 1\n") {
			t.Fatal("metric missing")
		}
		srv.blob.Store(readBlob(t, kvBlob(t, 50)))
		srv.etagNo.Store(0)
	})

	t.Run("cached file is re-verified on restart", func(t *testing.T) {
		srv.sigFn.Store(sign(priv, "k1", "ds-1"))
		dir := t.TempDir()
		s, err := open(dir, keys)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
		// Server now unreachable for signed data: restart must still serve the verified cache.
		srv.sigFn.Store(sign(otherPriv, "k1", "ds-1"))
		s2, err := open(dir, keys)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s2.Get([]byte("k7")); err != nil {
			t.Fatal(err)
		}
		s2.Close()
		// Tampering with the cache (signature removed) rejects it.
		if err := os.Remove(s.sigPath()); err != nil {
			t.Fatal(err)
		}
		if s3, err := open(dir, keys); err == nil {
			s3.Close()
			t.Fatal("cache without signature must be rejected when the server also fails verification")
		}
	})
}
