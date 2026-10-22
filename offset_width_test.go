package attos

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type goldenW5 struct {
	n, bs int // bs == 0: uncompressed
}

var goldenW5Files = []goldenW5{{1, 0}, {1000, 0}, {5000, 0}, {1, 16}, {1000, 16}}

func goldenW5Name(g goldenW5) string {
	if g.bs == 0 {
		return filepath.Join("testdata", fmt.Sprintf("golden_v2_w5_%d.attos", g.n))
	}
	return filepath.Join("testdata", fmt.Sprintf("golden_v2_w5_lz4b%d_%d.attos", g.bs, g.n))
}

func w5Options(g goldenW5) CompileOptions {
	o := CompileOptions{OffsetWidth: 5}
	if g.bs != 0 {
		o.Compression, o.BlockSize = CompressLZ4, g.bs
	}
	return o
}

func TestGenerateGoldenW5(t *testing.T) {
	if os.Getenv("GEN_GOLDEN") == "" {
		t.Skip("set GEN_GOLDEN=1 to regenerate testdata")
	}
	for _, g := range goldenW5Files {
		b := NewBuilder()
		for _, e := range goldenEntries(g.n) {
			if err := b.Add(e.key, e.value, 0); err != nil {
				t.Fatal(err)
			}
		}
		if err := b.CompileWithOptions(goldenW5Name(g), w5Options(g)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGoldenW5Readable(t *testing.T) {
	for _, g := range goldenW5Files {
		m, err := OpenWithConfig(goldenW5Name(g), OpenConfig{ValidateChecksum: true})
		if err != nil {
			t.Fatalf("%+v: %v", g, err)
		}
		if mp := m.cur.Load(); mp.offW != 5 {
			t.Fatalf("%+v: offset width %d", g, mp.offW)
		}
		for _, e := range goldenEntries(g.n) {
			v, err := m.Get(e.key)
			if err != nil || !bytes.Equal(v, e.value) {
				t.Fatalf("%+v key %q: %v", g, e.key, err)
			}
		}
		m.Close()
	}
}

// Forcing width 5 on small data must read back identically and be selected only on request.
func TestOffsetWidthSelection(t *testing.T) {
	dir := t.TempDir()
	for _, bs := range []int{0, 8} {
		for _, w := range []int{0, 4, 5} {
			b := NewBuilder()
			for _, e := range goldenEntries(777) {
				b.Add(e.key, e.value, 0)
			}
			opts := CompileOptions{OffsetWidth: w}
			if bs != 0 {
				opts.Compression, opts.BlockSize = CompressLZ4, bs
			}
			p := filepath.Join(dir, fmt.Sprintf("w%d_%d.attos", w, bs))
			if err := b.CompileWithOptions(p, opts); err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(p)
			wantHdr := byte(0)
			if w == 5 {
				wantHdr = 5
			}
			if raw[52] != wantHdr {
				t.Fatalf("bs=%d w=%d: header byte 52 = %d", bs, w, raw[52])
			}
			m, err := OpenWithConfig(p, OpenConfig{ValidateChecksum: true})
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range goldenEntries(777) {
				if v, err := m.Get(e.key); err != nil || !bytes.Equal(v, e.value) {
					t.Fatalf("bs=%d w=%d key %q: %v", bs, w, e.key, err)
				}
			}
			n := 0
			m.Scan(func(k, v []byte) bool { n++; return true })
			if n != 777 {
				t.Fatalf("scan %d", n)
			}
			m.Close()
		}
	}
	b := NewBuilder()
	b.Add([]byte("k"), []byte("v"), 0)
	if err := b.CompileWithOptions(filepath.Join(dir, "bad"), CompileOptions{OffsetWidth: 3}); err == nil {
		t.Fatal("width 3 accepted")
	}
}

// A reader must reject an unknown offset width.
func TestOffsetWidthCorrupt(t *testing.T) {
	g := goldenW5Files[1]
	raw, err := os.ReadFile(goldenW5Name(g))
	if err != nil {
		t.Fatal(err)
	}
	raw[52] = 7
	p := filepath.Join(t.TempDir(), "w7.attos")
	os.WriteFile(p, raw, 0o644)
	if _, err := Open(p); err == nil {
		t.Fatal("width 7 accepted")
	}
}

// TestBlobOver4GiB builds a >4 GiB blob and proves width 5 is chosen automatically.
// Needs about 4.5 GiB of RAM and disk: ATTOS_BIG_TEST=1.
func TestBlobOver4GiB(t *testing.T) {
	if os.Getenv("ATTOS_BIG_TEST") == "" {
		t.Skip("set ATTOS_BIG_TEST=1 (uses ~4.5 GiB RAM and disk)")
	}
	const rows = 4100
	val := func(i int) []byte {
		v := make([]byte, MaxValueLen)
		copy(v, fmt.Sprintf("value-%d", i))
		v[len(v)-1] = byte(i)
		return v
	}
	b := NewBuilder()
	for i := 0; i < rows; i++ {
		if err := b.Add([]byte(fmt.Sprintf("big/%05d", i)), val(i), 0); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(os.TempDir(), "attos_over4gib.attos")
	defer os.Remove(p)
	if err := b.CompileWithOptions(p, CompileOptions{OffsetWidth: 4}); err == nil {
		t.Fatal("forced width 4 must fail on a >4 GiB blob")
	}
	if err := b.Compile(p); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Size() < 1<<32 {
		t.Fatalf("file only %d bytes", fi.Size())
	}
	m, err := OpenWithConfig(p, OpenConfig{ValidateChecksum: true})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if mp := m.cur.Load(); mp.offW != 5 {
		t.Fatalf("offset width %d, want 5", mp.offW)
	}
	for i := 0; i < rows; i++ {
		v, err := m.Get([]byte(fmt.Sprintf("big/%05d", i)))
		if err != nil || len(v) != MaxValueLen || !bytes.Equal(v[:16], val(i)[:16]) || v[len(v)-1] != byte(i) {
			t.Fatalf("row %d: %v len=%d", i, err, len(v))
		}
	}
	if _, err := m.Get([]byte("big/99999")); err == nil {
		t.Fatal("absent key found")
	}
	t.Logf("file %d bytes, width 5, %d rows ok", fi.Size(), rows)
}
