package attos_test

import (
	"bufio"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	attos "github.com/Attos-Labs/attos-go"
)

// GetCopy and View must not allocate on LZ4 files (Get still copies the record, by design).
func TestCompressedGetCopyAndViewAllocateNothing(t *testing.T) {
	m, err := attos.Open("testdata/golden_v2_lz4b16_1000.attos")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	f, err := os.Open("testdata/golden_v2_1000.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var keys, vals [][]byte
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.SplitN(sc.Text(), "\t", 2)
		if len(p) == 2 { // the fixture stores key and value hex-encoded
			k, err1 := hex.DecodeString(p[0])
			v, err2 := hex.DecodeString(p[1])
			if err1 != nil || err2 != nil {
				t.Fatalf("bad fixture line %q", sc.Text())
			}
			keys = append(keys, k)
			vals = append(vals, v)
		}
	}
	if len(keys) == 0 {
		t.Fatal("no fixture rows")
	}
	buf := make([]byte, 0, 256)
	for i, k := range keys { // correctness first
		got, err := m.GetCopy(k, buf[:0])
		if err != nil || string(got) != string(vals[i]) {
			t.Fatalf("GetCopy(%q) = %q, %v; want %q", k, got, err, vals[i])
		}
		var viewed string
		if err := m.View(k, func(v []byte) { viewed = string(v) }); err != nil || viewed != string(vals[i]) {
			t.Fatalf("View(%q) = %q, %v; want %q", k, viewed, err, vals[i])
		}
	}
	i := 0
	n := testing.AllocsPerRun(2000, func() {
		_, _ = m.GetCopy(keys[i%len(keys)], buf[:0])
		_ = m.View(keys[i%len(keys)], func([]byte) {})
		i++
	})
	if n != 0 && !raceEnabled {
		t.Fatalf("GetCopy+View allocated %.2f per run on an LZ4 file, want 0", n)
	}
}
