package attos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
)

// The lean-key variant of TestOverflowGrowsUnderConcurrentReads: a lean main (8-byte fingerprints) grows through
// tail patches delivered by the real Synchronizer while eight readers hammer it, then the chain restarts on a new
// lean main (a compaction), delivered as a full blob, and the readers keep going.
func TestLeanOverflowGrowsUnderConcurrentReads(t *testing.T) {
	const n = 3000
	base := buildMode(t, n, CompileOptions{FingerprintBytes: 8}) // lean: key_i -> val_i
	d := newDeltaServer(t, base)
	s := syncer(t, d)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var reads, bad atomic.Uint64
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
				k := 100 + i%(n-100) // keys 0..99 are the ones the generations edit
				v, err := s.Get([]byte(fmt.Sprintf("key_%d", k)))
				reads.Add(1)
				if err != nil || string(v) != fmt.Sprintf("val_%d", k) {
					bad.Add(1)
				}
				if v, err := s.Get([]byte("key_1")); err == nil && string(v) != "val_1" && string(v) != "edited" {
					bad.Add(1)
				}
				// a key that is in no version must never resolve, whatever the fingerprint says
				if _, err := s.Get([]byte(fmt.Sprintf("absent_%d", i))); err == nil {
					bad.Add(1)
				}
			}
		}(g)
	}
	items := []ovItem{}
	cur := base
	for gen := 1; gen <= 4; gen++ {
		items = append(items, ovItem{key: []byte(fmt.Sprintf("ins_%d", gen)), val: []byte("x")})
		if gen == 1 {
			items = append(items, ovItem{key: []byte("key_1"), val: []byte("edited")})
		}
		if gen == 2 {
			items = append(items, ovItem{key: []byte("key_2"), tomb: true})
		}
		cp := append([]ovItem(nil), items...)
		nw := withOverflow(t, base, cp)
		if nw[8] != 0x06 {
			t.Fatalf("gen %d: flags %#x", gen, nw[8])
		}
		d.publishWith(nw, makeTailPatch)
		if err := s.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
		cur = nw
	}
	// compaction: a new lean main without overflow, sent as a full blob (no patch relates it to the old chain)
	next := buildMode(t, n+50, CompileOptions{FingerprintBytes: 8})
	d.set(next, `"compacted"`)
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d bad reads of %d", bad.Load(), reads.Load())
	}
	if a, _, _ := s.DeltaStats(); a != 4 {
		t.Fatalf("applied=%d, want 4 tail patches", a)
	}
	wantValue(t, s, "key_1", "val_1") // the compacted main is a plain rebuild of key_i -> val_i
	wantValue(t, s, fmt.Sprintf("key_%d", n+49), fmt.Sprintf("val_%d", n+49))
	if _, err := s.Get([]byte("ins_3")); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("ins_3 after compaction: %v", err)
	}
	if got, _ := os.ReadFile(s.livePath()); !bytes.Equal(got, next) {
		t.Fatal("live file differs from the published blob")
	}
	_ = cur
	t.Logf("%d reads across 4 lean overflow generations and a compaction, 0 bad", reads.Load())
}
