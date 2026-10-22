package attos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fillBuilder(t testing.TB, b *Builder, n int, vlen int) {
	t.Helper()
	val := bytes.Repeat([]byte("v"), vlen)
	for i := 0; i < n; i++ {
		v := append([]byte(fmt.Sprintf("%d:", i)), val...)
		if err := b.Add([]byte(fmt.Sprintf("tenant%d\x00us-east\x00row%08d", i%977, i)), v, i+1); err != nil {
			t.Fatal(err)
		}
	}
}

// The spilling builder must produce exactly the same bytes as the in-memory one.
func TestSpillBuilderIdentical(t *testing.T) {
	dir := t.TempDir()
	for _, opts := range []CompileOptions{{}, {OffsetWidth: 5}, {Compression: CompressLZ4}, {Compression: CompressLZ4, BlockSize: 64, OffsetWidth: 5}} {
		mem := NewBuilder()
		fillBuilder(t, mem, 20000, 50)
		sp, err := NewSpillBuilder(dir)
		if err != nil {
			t.Fatal(err)
		}
		fillBuilder(t, sp, 20000, 50)
		a, b := filepath.Join(dir, "a.attos"), filepath.Join(dir, "b.attos")
		if err := mem.CompileWithOptions(a, opts); err != nil {
			t.Fatal(err)
		}
		if err := sp.CompileWithOptions(b, opts); err != nil {
			t.Fatal(err)
		}
		ba, _ := os.ReadFile(a)
		bb, _ := os.ReadFile(b)
		if !bytes.Equal(ba, bb) {
			t.Fatalf("%+v: spill output differs (%d vs %d bytes)", opts, len(ba), len(bb))
		}
		sp.Close()
	}
	left, _ := filepath.Glob(filepath.Join(dir, "attos-*"))
	if len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

func TestSpillBuilderRollbackAndDuplicate(t *testing.T) {
	dir := t.TempDir()
	b, err := NewSpillBuilder(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	good := "id,attos_value\na,1\nb,2\n"
	if err := b.BuildFrom(context.Background(), strings.NewReader(good), FormatAuto); err != nil {
		t.Fatal(err)
	}
	// a malformed second batch must roll back without disturbing the first rows
	if err := b.BuildFrom(context.Background(), strings.NewReader("id,attos_value\nc,3\n\"bad,4\n"), FormatAuto); err == nil {
		t.Fatal("expected a parse error")
	}
	if b.Len() != 2 {
		t.Fatalf("rows after rollback: %d", b.Len())
	}
	if err := b.BuildFrom(context.Background(), strings.NewReader("id,attos_value\nd,4\n"), FormatAuto); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "o.attos")
	if err := b.Compile(out); err != nil {
		t.Fatal(err)
	}
	m, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"a": "1", "b": "2", "d": "4"} {
		if v, err := m.Get([]byte(k)); err != nil || string(v) != want {
			t.Fatalf("%s: %q %v", k, v, err)
		}
	}
	if _, err := m.Get([]byte("c")); err == nil {
		t.Fatal("rolled-back row present")
	}
	m.Close()

	d, _ := NewSpillBuilder(dir)
	defer d.Close()
	d.Add([]byte("same"), []byte("1"), 1)
	d.Add([]byte("other"), []byte("1"), 2)
	d.Add([]byte("same"), []byte("2"), 3)
	err = d.Compile(filepath.Join(dir, "d.attos"))
	var de *DuplicateKeyError
	if !errors.As(err, &de) || de.Key != "same" || de.Line != 3 || de.FirstLine != 1 || !strings.Contains(err.Error(), `"same"`) {
		t.Fatalf("duplicate error: %v", err)
	}
}

// Peak heap during build and compile must stay near the index size, not the data size.
func TestSpillBuilderMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("memory regression test")
	}
	const n, vlen = 1_000_000, 300
	dir := t.TempDir()
	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > peak.Load() {
				peak.Store(ms.HeapAlloc)
			}
		}
	}()
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	b, err := NewSpillBuilder(dir)
	if err != nil {
		t.Fatal(err)
	}
	fillBuilder(t, b, n, vlen)
	if err := b.Compile(filepath.Join(dir, "o.attos")); err != nil {
		t.Fatal(err)
	}
	b.Close()
	close(stop)
	<-done
	per := float64(peak.Load()-base.HeapAlloc) / n
	t.Logf("spill builder peak heap: %.1f B/key (value %d B)", per, vlen)
	if per > 75 {
		t.Fatalf("peak heap %.1f B/key exceeds 75 B/key", per)
	}
}
