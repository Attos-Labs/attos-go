package attos

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// mappedRSS returns the resident bytes of the first mapping of path in /proc/self/smaps.
func mappedRSS(t *testing.T, path string) int64 {
	t.Helper()
	f, err := os.Open("/proc/self/smaps")
	if err != nil {
		t.Skip("no /proc/self/smaps")
	}
	defer f.Close()
	abs, _ := filepath.Abs(path)
	sc := bufio.NewScanner(f)
	in := false
	for sc.Scan() {
		l := sc.Text()
		if strings.HasSuffix(l, abs) {
			in = true
			continue
		}
		if in && strings.HasPrefix(l, "Rss:") {
			kb, _ := strconv.ParseInt(strings.Fields(l)[1], 10, 64)
			return kb * 1024
		}
	}
	t.Fatal("mapping not found")
	return 0
}

// A default Open (with or without checksum validation) must not leave the whole file resident;
// WarmUp: true must.
func TestOpenRSS(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	dir := t.TempDir()
	b := NewBuilder()
	fillBuilder(t, b, 150_000, 300)
	p := filepath.Join(dir, "r.attos")
	if err := b.Compile(p); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	size := fi.Size()
	for _, c := range []struct {
		name string
		cfg  OpenConfig
		full bool
	}{
		{"default", OpenConfig{}, false},
		{"validate", OpenConfig{ValidateChecksum: true}, false},
		{"warmup", OpenConfig{WarmUp: true}, true},
	} {
		m, err := OpenWithConfig(p, c.cfg)
		if err != nil {
			t.Fatal(err)
		}
		rss := mappedRSS(t, p)
		m.Close()
		frac := float64(rss) / float64(size)
		t.Logf("%-8s resident %d of %d bytes (%.1f%%)", c.name, rss, size, frac*100)
		if c.full && frac < 0.9 {
			t.Fatalf("%s: expected the whole file resident, got %.1f%%", c.name, frac*100)
		}
		if !c.full && frac > 0.10 {
			t.Fatalf("%s: %.1f%% of the file resident after open, want < 10%%", c.name, frac*100)
		}
	}
	_ = fmt.Sprint
}
