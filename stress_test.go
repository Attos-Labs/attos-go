package attos

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const stressKeys = 2000

// genBlob builds generation g: key_i → "g<g>|key_i|<pad>" where the pad length
// and byte differ per generation, so a torn read (bytes from two different
// mappings, or from an unmapped/reused region) cannot validate.
func genBlob(t testing.TB, g int) []byte {
	var in strings.Builder
	in.WriteString("id,attos_value\n")
	pad := strings.Repeat(string(rune('a'+g)), 8+g*5)
	for i := 0; i < stressKeys; i++ {
		fmt.Fprintf(&in, "key_%d,g%d|key_%d|%s\n", i, g, i, pad)
	}
	var out bytes.Buffer
	if _, err := Compile(context.Background(), strings.NewReader(in.String()), FormatAuto, &out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func checkGenValue(key string, v []byte) error {
	s := string(v)
	parts := strings.SplitN(s, "|", 3)
	if len(parts) != 3 || len(parts[0]) < 2 || parts[0][0] != 'g' || parts[1] != key {
		return fmt.Errorf("torn/foreign value for %s: %q", key, s)
	}
	g, err := strconv.Atoi(parts[0][1:])
	if err != nil {
		return fmt.Errorf("bad gen in %q", s)
	}
	if parts[2] != strings.Repeat(string(rune('a'+g)), 8+g*5) {
		return fmt.Errorf("torn pad for %s: %q", key, s)
	}
	return nil
}

func countFDs(t testing.TB) int {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc/self/fd")
	}
	return len(ents)
}

func countMaps(t testing.TB, dir string) int {
	b, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Skip("no /proc/self/maps")
	}
	return strings.Count(string(b), dir)
}

type readerPool struct {
	stop    atomic.Bool
	wg      sync.WaitGroup
	lookups atomic.Uint64
	errs    atomic.Uint64
	firstMu sync.Mutex
	first   error
}

func (p *readerPool) fail(err error) {
	p.errs.Add(1)
	p.firstMu.Lock()
	if p.first == nil {
		p.first = err
	}
	p.firstMu.Unlock()
}

// run starts n readers doing continuous lookups through View, GetCopy and Get.
func (p *readerPool) run(n int, view func([]byte, func([]byte)) error, getCopy func(k, dst []byte) ([]byte, error)) {
	for r := 0; r < n; r++ {
		p.wg.Add(1)
		go func(r int) {
			defer p.wg.Done()
			rng := rand.New(rand.NewPCG(uint64(r), 99))
			key := make([]byte, 0, 32)
			var dst []byte
			var local uint64
			for !p.stop.Load() {
				i := rng.IntN(stressKeys)
				key = append(key[:0], "key_"...)
				key = strconv.AppendInt(key, int64(i), 10)
				ks := string(key)
				var cerr error
				err := view(key, func(v []byte) { cerr = checkGenValue(ks, v) })
				if err != nil || cerr != nil {
					p.fail(fmt.Errorf("view %s: %v %v", ks, err, cerr))
				}
				dst, err = getCopy(key, dst[:0])
				if err != nil {
					p.fail(fmt.Errorf("getcopy %s: %v", ks, err))
				} else if err := checkGenValue(ks, dst); err != nil {
					p.fail(err)
				}
				local += 2
			}
			p.lookups.Add(local)
		}(r)
	}
}

// TestHotSwapStress: 32 readers doing continuous lookups while the map is
// swapped ≥ 1000 times with no grace period (retirement relies purely on the
// reader-drain handshake). Zero torn reads, zero errors, ≤ 2 pending retired
// maps at any time, and FDs / mapped regions back to baseline afterwards.
func TestHotSwapStress(t *testing.T) {
	dir := t.TempDir()
	const gens = 4
	paths := make([]string, gens)
	for g := range paths {
		paths[g] = filepath.Join(dir, fmt.Sprintf("gen%d.attos", g))
		if err := os.WriteFile(paths[g], genBlob(t, g), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fds0 := countFDs(t)
	m, err := OpenWithConfig(paths[0], OpenConfig{ValidateChecksum: true, RetireGrace: -1})
	if err != nil {
		t.Fatal(err)
	}
	var pool readerPool
	pool.run(32, m.View, m.GetCopy)

	swaps := 1000
	if testing.Short() {
		swaps = 200
	}
	maxPending, maxMaps := 0, 0
	start := time.Now()
	for i := 1; i <= swaps; i++ {
		if err := m.Reload(paths[i%gens]); err != nil {
			t.Fatalf("swap %d: %v", i, err)
		}
		if p := m.PendingRetired(); p > maxPending {
			maxPending = p
		}
		if i%50 == 0 {
			if c := countMaps(t, dir); c > maxMaps {
				maxMaps = c
			}
		}
	}
	elapsed := time.Since(start)
	pool.stop.Store(true)
	pool.wg.Wait()
	if pool.errs.Load() != 0 {
		t.Fatalf("%d reader errors; first: %v", pool.errs.Load(), pool.first)
	}
	if maxPending > maxRetired {
		t.Fatalf("pending retired maps reached %d (> %d)", maxPending, maxRetired)
	}
	if maxMaps > 1+maxRetired {
		t.Fatalf("mapped regions reached %d (> %d)", maxMaps, 1+maxRetired)
	}
	if got := m.Swaps(); got != uint64(swaps) {
		t.Fatalf("swaps=%d", got)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if c := countMaps(t, dir); c != 0 {
		t.Fatalf("%d mappings of test files remain after Close", c)
	}
	if fds := countFDs(t); fds != fds0 {
		t.Fatalf("fd count %d after Close, baseline %d", fds, fds0)
	}
	t.Logf("swaps=%d in %v, 32 readers, lookups=%d (Map.Lookups=%d), reader errors=0, max pending retired=%d, max mapped regions=%d, fds %d→%d, maps→0",
		swaps, elapsed.Round(time.Millisecond), pool.lookups.Load(), m.Lookups(), maxPending, maxMaps, fds0, countFDs(t))
}

// TestSynchronizerSwapStress drives swaps through the full sync path
// (HTTP download → fsync → validate → rename → publish) under 32 readers.
func TestSynchronizerSwapStress(t *testing.T) {
	const gens = 3
	blobs := make([][]byte, gens)
	for g := range blobs {
		blobs[g] = genBlob(t, g)
	}
	f := newFakeServer(t)
	f.set(blobs[0], `"g0"`)
	dir := t.TempDir()
	fds0 := countFDs(t)
	s := newTestSync(t, f, dir, WithOpenConfig(OpenConfig{RetireGrace: -1}))
	var pool readerPool
	pool.run(32, s.View, s.GetCopy)
	swaps := 300
	if testing.Short() {
		swaps = 60
	}
	for i := 1; i <= swaps; i++ {
		f.set(blobs[i%gens], fmt.Sprintf(`"g%d-%d"`, i%gens, i))
		if err := s.Sync(context.Background()); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
		if p := s.Map().PendingRetired(); p > maxRetired {
			t.Fatalf("pending retired %d", p)
		}
	}
	pool.stop.Store(true)
	pool.wg.Wait()
	if pool.errs.Load() != 0 {
		t.Fatalf("%d reader errors; first: %v", pool.errs.Load(), pool.first)
	}
	if got := s.Map().Swaps(); got != uint64(swaps) {
		t.Fatalf("swaps=%d want %d", got, swaps)
	}
	s.Close()
	f.srv.CloseClientConnections()
	waitFor(t, func() bool { return countMaps(t, dir) == 0 })
	// HTTP keep-alive sockets are closed above; allow the runtime a moment.
	waitFor(t, func() bool { return countFDs(t) <= fds0 })
	t.Logf("sync swaps=%d, 32 readers, lookups=%d, reader errors=0, maps→0, fds back to %d", swaps, pool.lookups.Load(), fds0)
}
