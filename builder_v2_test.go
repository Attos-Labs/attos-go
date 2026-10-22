package attos_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Attos-Labs/attos-go"
)

func buildCSV(t *testing.T, n int) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("k,attos_value\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "key-%d,value-%d\n", i, i*7)
	}
	p := filepath.Join(t.TempDir(), "in.csv")
	if err := os.WriteFile(p, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestV2RoundTripAndMisses(t *testing.T) {
	for _, n := range []int{1, 2, 7, 1000, 50000} {
		b := attos.NewBuilder()
		if err := b.BuildFromCSV(buildCSV(t, n)); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(t.TempDir(), "v2.attos")
		if err := b.Compile(out); err != nil {
			t.Fatal(err)
		}
		m, err := attos.OpenWithConfig(out, attos.OpenConfig{ValidateChecksum: true})
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		for i := 0; i < n; i++ {
			v, err := m.GetString(fmt.Sprintf("key-%d", i))
			if err != nil || v != fmt.Sprintf("value-%d", i*7) {
				t.Fatalf("n=%d key %d: %q %v", n, i, v, err)
			}
		}
		for i := 0; i < 2000; i++ {
			if _, err := m.GetString(fmt.Sprintf("missing-%d", i)); err != attos.ErrKeyNotFound {
				t.Fatalf("n=%d: missing key returned %v", n, err)
			}
		}
		count := 0
		m.Scan(func(k, v []byte) bool { count++; return true })
		if count != n {
			t.Fatalf("n=%d: scan saw %d", n, count)
		}
		m.Close()
	}
}

func TestV2SmallerThanV1(t *testing.T) {
	csv := buildCSV(t, 50000)
	b := attos.NewBuilder()
	if err := b.BuildFromCSV(csv); err != nil {
		t.Fatal(err)
	}
	d := t.TempDir()
	if err := b.Compile(filepath.Join(d, "v2")); err != nil {
		t.Fatal(err)
	}
	if err := b.CompileV1(filepath.Join(d, "v1")); err != nil {
		t.Fatal(err)
	}
	s2, _ := os.Stat(filepath.Join(d, "v2"))
	s1, _ := os.Stat(filepath.Join(d, "v1"))
	t.Logf("v1=%d bytes v2=%d bytes (%.1f%%)", s1.Size(), s2.Size(), 100*float64(s2.Size())/float64(s1.Size()))
	if s2.Size() >= s1.Size() {
		t.Fatalf("v2 should be smaller")
	}
	// v1 files must still open and read
	m, err := attos.Open(filepath.Join(d, "v1"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if v, err := m.GetString("key-123"); err != nil || v != "value-861" {
		t.Fatalf("v1 read: %q %v", v, err)
	}
}

func TestV2CorruptHeaders(t *testing.T) {
	b := attos.NewBuilder()
	if err := b.BuildFromCSV(buildCSV(t, 100)); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "v2.attos")
	if err := b.Compile(out); err != nil {
		t.Fatal(err)
	}
	good, _ := os.ReadFile(out)
	for _, cut := range []int{10, 63, 64, 100, len(good) / 2, len(good) - 5} {
		p := filepath.Join(t.TempDir(), "cut")
		os.WriteFile(p, good[:cut], 0o644)
		if m, err := attos.Open(p); err == nil {
			// opening a truncated tail may succeed only if the structure is intact; lookups must never panic
			for i := 0; i < 100; i++ {
				m.GetString(fmt.Sprintf("key-%d", i))
			}
			m.Close()
		}
	}
	bad := append([]byte(nil), good...)
	bad[9] = 0 // front width 0 is invalid
	p := filepath.Join(t.TempDir(), "bad")
	os.WriteFile(p, bad, 0o644)
	if _, err := attos.Open(p); err == nil {
		t.Fatal("expected error for invalid width")
	}
}

func TestV2CompressedRoundTrip(t *testing.T) {
	for _, bs := range []int{0, 1, 2, 16, 64, 256} {
		for _, n := range []int{1, 2, 17, 1000, 20000} {
			b := attos.NewBuilder()
			if err := b.BuildFromCSV(buildCSV(t, n)); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "c.attos")
			if err := b.CompileWithOptions(out, attos.CompileOptions{Compression: attos.CompressLZ4, BlockSize: bs}); err != nil {
				t.Fatal(err)
			}
			m, err := attos.OpenWithConfig(out, attos.OpenConfig{ValidateChecksum: true})
			if err != nil {
				t.Fatalf("bs=%d n=%d: %v", bs, n, err)
			}
			for i := 0; i < n; i++ {
				v, err := m.GetString(fmt.Sprintf("key-%d", i))
				if err != nil || v != fmt.Sprintf("value-%d", i*7) {
					t.Fatalf("bs=%d n=%d key %d: %q %v", bs, n, i, v, err)
				}
			}
			for i := 0; i < 500; i++ {
				if _, err := m.GetString(fmt.Sprintf("missing-%d", i)); err != attos.ErrKeyNotFound {
					t.Fatalf("missing key: %v", err)
				}
			}
			cnt := 0
			m.Scan(func(k, v []byte) bool { cnt++; return true })
			if cnt != n {
				t.Fatalf("bs=%d n=%d scan=%d", bs, n, cnt)
			}
			m.Close()
		}
	}
}

func TestV2CompressedIsSmallerAndRejectsBadBlockSize(t *testing.T) {
	b := attos.NewBuilder()
	if err := b.BuildFromCSV(buildCSV(t, 20000)); err != nil {
		t.Fatal(err)
	}
	d := t.TempDir()
	if err := b.Compile(filepath.Join(d, "raw")); err != nil {
		t.Fatal(err)
	}
	if err := b.CompileWithOptions(filepath.Join(d, "lz4"), attos.CompileOptions{Compression: attos.CompressLZ4}); err != nil {
		t.Fatal(err)
	}
	r, _ := os.Stat(filepath.Join(d, "raw"))
	c, _ := os.Stat(filepath.Join(d, "lz4"))
	t.Logf("raw=%d lz4=%d", r.Size(), c.Size())
	if c.Size() >= r.Size() {
		t.Fatal("compressed file should be smaller")
	}
	if err := b.CompileWithOptions(filepath.Join(d, "x"), attos.CompileOptions{Compression: attos.CompressLZ4, BlockSize: 3}); err == nil {
		t.Fatal("expected error for block size 3")
	}
}

func TestV2CompressedCorruption(t *testing.T) {
	b := attos.NewBuilder()
	if err := b.BuildFromCSV(buildCSV(t, 2000)); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "c.attos")
	if err := b.CompileWithOptions(out, attos.CompileOptions{Compression: attos.CompressLZ4}); err != nil {
		t.Fatal(err)
	}
	good, _ := os.ReadFile(out)
	for i := 0; i < 3000; i++ {
		bad := append([]byte(nil), good...)
		for j := 0; j < 1+i%4; j++ {
			bad[(i*7919+j*104729)%len(bad)] ^= byte(1 + i%255)
		}
		p := filepath.Join(t.TempDir(), "bad")
		os.WriteFile(p, bad, 0o644)
		m, err := attos.Open(p)
		if err != nil {
			continue
		}
		for k := 0; k < 50; k++ {
			m.GetString(fmt.Sprintf("key-%d", k)) // must not panic
		}
		m.Scan(func(k, v []byte) bool { return true })
		m.Close()
	}
}
