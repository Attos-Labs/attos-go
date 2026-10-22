package attos

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type goldenLean struct{ n, fp, bs int } // bs == 0: uncompressed

var goldenLeanFiles = []goldenLean{{1, 4, 0}, {1000, 4, 0}, {1000, 8, 0}, {1000, 4, 16}, {1000, 8, 16}}

func goldenLeanName(g goldenLean) string {
	if g.bs == 0 {
		return filepath.Join("testdata", fmt.Sprintf("golden_v2_lean%d_%d.attos", g.fp, g.n))
	}
	return filepath.Join("testdata", fmt.Sprintf("golden_v2_lean%d_lz4b%d_%d.attos", g.fp, g.bs, g.n))
}

func leanOptions(g goldenLean) CompileOptions {
	o := CompileOptions{FingerprintBytes: g.fp}
	if g.bs != 0 {
		o.Compression, o.BlockSize = CompressLZ4, g.bs
	}
	return o
}

func TestGenerateGoldenLean(t *testing.T) {
	if os.Getenv("GEN_GOLDEN") == "" {
		t.Skip("set GEN_GOLDEN=1 to regenerate testdata")
	}
	for _, g := range goldenLeanFiles {
		b := NewBuilder()
		for _, e := range goldenEntries(g.n) {
			if err := b.Add(e.key, e.value, 0); err != nil {
				t.Fatal(err)
			}
		}
		if err := b.CompileWithOptions(goldenLeanName(g), leanOptions(g)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGoldenLeanReadable(t *testing.T) {
	for _, g := range goldenLeanFiles {
		m, err := OpenWithConfig(goldenLeanName(g), OpenConfig{ValidateChecksum: true})
		if err != nil {
			t.Fatalf("%+v: %v", g, err)
		}
		for _, e := range goldenEntries(g.n) {
			v, err := m.Get(e.key)
			if err != nil || !bytes.Equal(v, e.value) {
				t.Fatalf("%+v key %q: %v", g, e.key, err)
			}
			if _, err := m.Get(append(append([]byte(nil), e.key...), 'x')); err == nil {
				t.Fatalf("%+v: absent key accepted", g)
			}
		}
		rows := 0
		m.Scan(func(k, v []byte) bool {
			if len(k) != g.fp {
				t.Fatalf("scan key length %d, want fingerprint %d", len(k), g.fp)
			}
			rows++
			return true
		})
		if rows != g.n {
			t.Fatalf("%+v: scan %d rows", g, rows)
		}
		m.Close()
	}
}

// The builder still rejects true duplicates, lean files combine with every other option, and absent
// keys are rejected (false-hit rate 2^-32; none expected among 200k probes).
func TestLeanKeysAllOptions(t *testing.T) {
	dir := t.TempDir()
	for _, fp := range []int{4, 8} {
		for _, bs := range []int{0, 8} {
			for _, w := range []int{0, 5} {
				b := NewBuilder()
				fillBuilder(t, b, 20000, 40)
				opts := CompileOptions{FingerprintBytes: fp, OffsetWidth: w}
				if bs != 0 {
					opts.Compression, opts.BlockSize = CompressLZ4, bs
				}
				p := filepath.Join(dir, "l.attos")
				if err := b.CompileWithOptions(p, opts); err != nil {
					t.Fatal(err)
				}
				m, err := OpenWithConfig(p, OpenConfig{ValidateChecksum: true})
				if err != nil {
					t.Fatal(err)
				}
				val := bytes.Repeat([]byte("v"), 40)
				for i := 0; i < 20000; i++ {
					k := []byte(fmt.Sprintf("tenant%d\x00us-east\x00row%08d", i%977, i))
					want := append([]byte(fmt.Sprintf("%d:", i)), val...)
					if v, err := m.Get(k); err != nil || !bytes.Equal(v, want) {
						t.Fatalf("fp=%d bs=%d w=%d key %d: %v", fp, bs, w, i, err)
					}
					if _, err := m.Get(append(k, '!')); err == nil {
						t.Fatalf("fp=%d bs=%d w=%d: absent key %d accepted", fp, bs, w, i)
					}
				}
				m.Close()
			}
		}
	}
	d := NewBuilder()
	d.Add([]byte("same"), []byte("1"), 1)
	d.Add([]byte("same"), []byte("2"), 2)
	if err := d.CompileWithOptions(filepath.Join(dir, "d.attos"), CompileOptions{FingerprintBytes: 4}); err == nil {
		t.Fatal("duplicate keys accepted in lean mode")
	}
	b := NewBuilder()
	b.Add([]byte("k"), []byte("v"), 1)
	if err := b.CompileWithOptions(filepath.Join(dir, "x.attos"), CompileOptions{FingerprintBytes: 5}); err == nil {
		t.Fatal("fingerprint width 5 accepted")
	}
}

// Lean mode must shrink the file by about (keyLen - fingerprint) bytes per record.
func TestLeanKeysSmaller(t *testing.T) {
	dir := t.TempDir()
	sizes := map[int]int64{}
	for _, fp := range []int{0, 4} {
		b := NewBuilder()
		fillBuilder(t, b, 50000, 100)
		p := filepath.Join(dir, fmt.Sprintf("s%d.attos", fp))
		if err := b.CompileWithOptions(p, CompileOptions{FingerprintBytes: fp}); err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Stat(p)
		sizes[fp] = fi.Size()
	}
	save := float64(sizes[0]-sizes[4]) / 50000
	t.Logf("full keys %d B, lean-4 %d B, saving %.1f B/key", sizes[0], sizes[4], save)
	if save < 20 {
		t.Fatalf("saving %.1f B/key, expected about key length - 4", save)
	}
}
