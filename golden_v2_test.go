package attos

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenEntries is the dataset behind testdata/golden_v2_*.attos. The Java and Rust SDK tests
// read these files and verify every row of the matching .tsv (hex key, hex value).
type goldenEntry struct{ key, value []byte }

func goldenEntries(n int) []goldenEntry {
	es := make([]goldenEntry, 0, n)
	for i := 0; i < n; i++ {
		var k, v []byte
		switch i % 10 {
		case 0:
			k = []byte(fmt.Sprintf("key-%d", i))
			v = []byte{} // empty value
		case 1:
			k = []byte(fmt.Sprintf("k\x00%d\x00z", i)) // NUL bytes inside key (dimension separator)
			v = []byte(fmt.Sprintf("v%d", i))
		case 2:
			k = []byte(fmt.Sprintf("ключ-日本語-%d", i)) // non-ASCII
			v = []byte(fmt.Sprintf("значение-%d", i))
		case 3:
			k = []byte(strings.Repeat("L", 300) + fmt.Sprint(i)) // long key
			v = []byte(strings.Repeat("x", 1000+i%50))           // long value
		default:
			k = []byte(fmt.Sprintf("key-%d", i))
			v = []byte(fmt.Sprintf("value-%d", i*7))
		}
		es = append(es, goldenEntry{key: k, value: v})
	}
	return es
}

func TestGenerateGolden(t *testing.T) {
	if os.Getenv("GEN_GOLDEN") == "" {
		t.Skip("set GEN_GOLDEN=1 to regenerate testdata")
	}
	os.MkdirAll("testdata", 0o755)
	for _, n := range []int{1, 1000, 5000} {
		b := NewBuilder()
		for _, e := range goldenEntries(n) {
			if err := b.Add(e.key, e.value, 0); err != nil {
				t.Fatal(err)
			}
		}
		base := filepath.Join("testdata", fmt.Sprintf("golden_v2_%d", n))
		if err := b.Compile(base + ".attos"); err != nil {
			t.Fatal(err)
		}
		f, _ := os.Create(base + ".tsv")
		w := bufio.NewWriter(f)
		for _, e := range goldenEntries(n) {
			fmt.Fprintf(w, "%x\t%x\n", e.key, e.value)
		}
		w.Flush()
		f.Close()
	}
}

type goldenLZ4 struct{ n, bs int }

var goldenLZ4Files = []goldenLZ4{{1, 16}, {1000, 1}, {1000, 16}, {1000, 256}, {5000, 16}}

func goldenLZ4Name(g goldenLZ4) string {
	return filepath.Join("testdata", fmt.Sprintf("golden_v2_lz4b%d_%d.attos", g.bs, g.n))
}

func TestGenerateGoldenLZ4(t *testing.T) {
	if os.Getenv("GEN_GOLDEN") == "" {
		t.Skip("set GEN_GOLDEN=1 to regenerate testdata")
	}
	for _, g := range goldenLZ4Files {
		b := NewBuilder()
		for _, e := range goldenEntries(g.n) {
			if err := b.Add(e.key, e.value, 0); err != nil {
				t.Fatal(err)
			}
		}
		if err := b.CompileWithOptions(goldenLZ4Name(g), CompileOptions{Compression: CompressLZ4, BlockSize: g.bs}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGoldenLZ4Readable(t *testing.T) {
	for _, g := range goldenLZ4Files {
		m, err := OpenWithConfig(goldenLZ4Name(g), OpenConfig{ValidateChecksum: true})
		if err != nil {
			t.Skip("golden lz4 files not generated")
		}
		for _, e := range goldenEntries(g.n) {
			v, err := m.Get(e.key)
			if err != nil || string(v) != string(e.value) {
				t.Fatalf("%+v key %q: %v", g, e.key, err)
			}
		}
		m.Close()
	}
}

func TestGoldenReadable(t *testing.T) {
	for _, n := range []int{1, 1000, 5000} {
		p := filepath.Join("testdata", fmt.Sprintf("golden_v2_%d.attos", n))
		if _, err := os.Stat(p); err != nil {
			t.Skip("golden files not generated")
		}
		m, err := OpenWithConfig(p, OpenConfig{ValidateChecksum: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range goldenEntries(n) {
			v, err := m.Get(e.key)
			if err != nil || string(v) != string(e.value) {
				t.Fatalf("n=%d key %q: %v", n, e.key, err)
			}
		}
		m.Close()
	}
}
