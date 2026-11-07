package attos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cespare/xxhash/v2"
)

type ovItem struct {
	key, val []byte
	tomb     bool
	hash     uint64 // 0 = XXH64(key); set only to force hash collisions
}

// withOverflow turns a v2 blob (with or without an overflow) into the same main blob plus an overflow
// block holding items: the layout of attos-cloud/docs/OVERFLOW_BLOCK.md, written independently of the
// readers under test.
func withOverflow(t testing.TB, blob []byte, items []ovItem) []byte {
	t.Helper()
	baseEnd := len(blob) - 4
	if blob[8]&flagOverflow != 0 {
		baseEnd = int(binary.BigEndian.Uint64(blob[len(blob)-16:]))
	}
	for i := range items {
		if items[i].hash == 0 {
			items[i].hash = xxhash.Sum64(items[i].key)
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].hash < items[j].hash })
	out := append([]byte(nil), blob[:baseEnd]...)
	out[8] |= flagOverflow
	le := binary.LittleEndian
	for _, it := range items {
		out = le.AppendUint64(out, it.hash)
	}
	var data []byte
	for _, it := range items {
		out = le.AppendUint64(out, uint64(len(data)))
		data = binary.BigEndian.AppendUint16(data, uint16(len(it.key)))
		if it.tomb {
			data = binary.BigEndian.AppendUint32(data, tombstoneLen)
			data = append(data, it.key...)
			continue
		}
		data = binary.BigEndian.AppendUint32(data, uint32(len(it.val)))
		data = append(data, it.key...)
		data = append(data, it.val...)
	}
	out = le.AppendUint64(out, uint64(len(data)))
	out = append(out, data...)
	out = binary.BigEndian.AppendUint64(out, uint64(len(items)))
	out = binary.BigEndian.AppendUint64(out, uint64(baseEnd))
	out = append(out, overflowMagic...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out))
}

func openBlob(t testing.TB, blob []byte) *Map {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.attos")
	if err := os.WriteFile(p, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := OpenWithConfig(p, OpenConfig{ValidateChecksum: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func buildMode(t testing.TB, n int, opts CompileOptions) []byte {
	t.Helper()
	b := NewBuilder()
	for i := 0; i < n; i++ {
		if err := b.Add([]byte(fmt.Sprintf("key_%d", i)), []byte(fmt.Sprintf("val_%d", i)), i+1); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(t.TempDir(), "b.attos")
	if err := b.CompileWithOptions(p, opts); err != nil {
		t.Fatal(err)
	}
	d, _ := os.ReadFile(p)
	return d
}

var overflowModes = map[string]CompileOptions{
	"plain": {},
	"lz4":   {Compression: CompressLZ4},
	"lean8": {FingerprintBytes: 8},
}

func TestOverflowSemantics(t *testing.T) {
	for name, opts := range overflowModes {
		t.Run(name, func(t *testing.T) {
			base := buildMode(t, 1000, opts)
			blob := withOverflow(t, base, []ovItem{
				{key: []byte("brand_new"), val: []byte("fresh")},   // insert
				{key: []byte("key_5"), val: []byte("OVERWRITTEN")}, // overwrite
				{key: []byte("key_7"), tomb: true},                 // delete a main key
				{key: []byte("never_existed"), tomb: true},         // tombstone for an absent key
				{key: []byte("key_9"), val: []byte("")},            // empty value is a value, not a delete
			})
			m := openBlob(t, blob)
			get := func(k string) (string, error) {
				v, err := m.GetCopy([]byte(k), nil)
				return string(v), err
			}
			if v, err := get("brand_new"); err != nil || v != "fresh" {
				t.Fatalf("insert: %q %v", v, err)
			}
			if v, err := get("key_5"); err != nil || v != "OVERWRITTEN" {
				t.Fatalf("overwrite: %q %v", v, err)
			}
			if _, err := get("key_7"); !errors.Is(err, ErrKeyNotFound) {
				t.Fatalf("tombstone must mask the main key: %v", err)
			}
			if _, err := get("never_existed"); !errors.Is(err, ErrKeyNotFound) {
				t.Fatalf("absent tombstone: %v", err)
			}
			if v, err := get("key_9"); err != nil || v != "" {
				t.Fatalf("empty overflow value: %q %v", v, err)
			}
			if v, err := get("key_6"); err != nil || v != "val_6" {
				t.Fatalf("untouched main key: %q %v", v, err)
			}
			if _, err := get("nope"); !errors.Is(err, ErrKeyNotFound) {
				t.Fatalf("miss: %v", err)
			}
			if got, want := m.Len(), 1000+1-1; got != want { // +brand_new, -key_7
				t.Fatalf("Len = %d, want %d", got, want)
			}
			if name == "lean8" {
				return // Scan reports fingerprints for main records in lean files
			}
			seen := map[string]string{}
			m.Scan(func(k, v []byte) bool { seen[string(k)] = string(v); return true })
			if len(seen) != 1000 {
				t.Fatalf("Scan saw %d keys", len(seen))
			}
			if _, ok := seen["key_7"]; ok || seen["key_5"] != "OVERWRITTEN" || seen["brand_new"] != "fresh" {
				t.Fatal("Scan does not reflect the overflow")
			}
		})
	}
}

func TestOverflowHashCollisionFallsThrough(t *testing.T) {
	// Entries that share key_5's hash but carry other keys (a crafted run; real XXH64 collisions cannot be
	// produced on demand): the lookup must skip them, then fall through to the main table or find its own.
	base := buildMode(t, 200, CompileOptions{})
	h5, h6 := xxhash.Sum64([]byte("key_5")), xxhash.Sum64([]byte("key_6"))
	blob := withOverflow(t, base, []ovItem{
		{key: []byte("collider_a"), val: []byte("A"), hash: h5},
		{key: []byte("collider_b"), val: []byte("B"), hash: h5},
		{key: []byte("key_6"), val: []byte("six"), hash: h6},
		{key: []byte("collider_c"), val: []byte("C"), hash: h6},
	})
	m := openBlob(t, blob)
	for k, want := range map[string]string{"key_5": "val_5", "key_6": "six", "key_7": "val_7"} {
		if v, err := m.GetCopy([]byte(k), nil); err != nil || string(v) != want {
			t.Fatalf("%s = %q, %v; want %q", k, v, err, want)
		}
	}
	// the key sits behind a mismatching entry of the same hash
	blob = withOverflow(t, base, []ovItem{
		{key: []byte("collider_a"), val: []byte("A"), hash: h5},
		{key: []byte("key_5"), val: []byte("mine"), hash: h5},
	})
	if v, err := openBlob(t, blob).GetCopy([]byte("key_5"), nil); err != nil || string(v) != "mine" {
		t.Fatalf("key behind a collider: %q %v", v, err)
	}
}

func TestOverflowZeroAllocs(t *testing.T) {
	base := buildMode(t, 1000, CompileOptions{})
	items := []ovItem{{key: []byte("new_key"), val: []byte("v")}, {key: []byte("key_7"), tomb: true}}
	for i := 0; i < 500; i++ {
		items = append(items, ovItem{key: []byte(fmt.Sprintf("ov_%d", i)), val: []byte("x")})
	}
	m := openBlob(t, withOverflow(t, base, items))
	for _, k := range []string{"new_key", "key_7", "key_8", "absent", "ov_250"} {
		kb := []byte(k)
		if a := testing.AllocsPerRun(200, func() { m.Get(kb) }); a != 0 {
			t.Errorf("Get(%s): %v allocs", k, a)
		}
	}
}

func TestOverflowOldFlagBitsStillRejected(t *testing.T) {
	base := buildMode(t, 100, CompileOptions{})
	b := append([]byte(nil), base...)
	b[8] |= 0x08 // a flag this SDK does not know
	binary.BigEndian.PutUint32(b[len(b)-4:], crc32.ChecksumIEEE(b[:len(b)-4]))
	p := filepath.Join(t.TempDir(), "x.attos")
	os.WriteFile(p, b, 0o600)
	var fe *FeatureError
	if _, err := OpenWithConfig(p, OpenConfig{ValidateChecksum: true}); !errors.As(err, &fe) {
		t.Fatalf("unknown flag must give a FeatureError: %v", err)
	}
}

// Every corruption of the overflow region (with a valid CRC, so only the structural checks can catch it)
// must be rejected or answered safely: never a panic, never an out-of-bounds read.
func TestOverflowHostileTail(t *testing.T) {
	base := buildMode(t, 100, CompileOptions{})
	blob := withOverflow(t, base, []ovItem{
		{key: []byte("a"), val: []byte("1")}, {key: []byte("b"), tomb: true}, {key: []byte("c"), val: []byte("333")},
	})
	start := int(binary.BigEndian.Uint64(blob[len(blob)-16:]))
	dir := t.TempDir()
	for i := start; i < len(blob)-4; i++ {
		for _, x := range []byte{0x01, 0x80, 0xff} {
			c := append([]byte(nil), blob...)
			c[i] ^= x
			binary.BigEndian.PutUint32(c[len(c)-4:], crc32.ChecksumIEEE(c[:len(c)-4]))
			p := filepath.Join(dir, "h.attos")
			os.WriteFile(p, c, 0o600)
			m, err := OpenWithConfig(p, OpenConfig{ValidateChecksum: true})
			if err != nil {
				continue
			}
			for _, k := range []string{"a", "b", "c", "key_1", "zzz"} {
				m.Get([]byte(k))
			}
			m.Len()
			m.Scan(func(k, v []byte) bool { return true })
			m.Close()
		}
	}
	// A truncated tail and a footer that lies about K are rejected outright.
	for _, cut := range []int{1, 8, 20, 30} {
		c := append([]byte(nil), blob[:len(blob)-4-cut]...)
		c = binary.BigEndian.AppendUint32(c, crc32.ChecksumIEEE(c))
		p := filepath.Join(dir, "t.attos")
		os.WriteFile(p, c, 0o600)
		if _, err := OpenWithConfig(p, OpenConfig{ValidateChecksum: true}); err == nil {
			t.Fatalf("tail cut %d accepted", cut)
		}
	}
	_ = bytes.Equal
}

// makeTailPatch is a test-only encoder for ATDL v2 (TRUNCATE_AND_APPEND): the new file is the main part of
// the old one plus new[baseEnd:].
func makeTailPatch(old, nw []byte) []byte {
	baseEnd := len(old) - 4
	if old[8]&flagOverflow != 0 {
		baseEnd = int(binary.BigEndian.Uint64(old[len(old)-16:]))
	}
	var b bytes.Buffer
	b.WriteString("ATDL")
	binary.Write(&b, binary.BigEndian, uint32(2))
	binary.Write(&b, binary.BigEndian, uint64(len(old)))
	b.Write(old[len(old)-4:])
	binary.Write(&b, binary.BigEndian, uint64(len(nw)))
	sum := sha256.Sum256(nw)
	b.Write(sum[:])
	b.WriteByte(1)
	binary.Write(&b, binary.BigEndian, uint64(baseEnd))
	binary.Write(&b, binary.BigEndian, uint64(len(nw)-baseEnd))
	b.Write(nw[baseEnd:])
	b.WriteByte(0)
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(b.Bytes()))
	return b.Bytes()
}

func TestTailPatchRebuildsTheFile(t *testing.T) {
	base := buildMode(t, 800, CompileOptions{})
	g1 := withOverflow(t, base, []ovItem{{key: []byte("n1"), val: []byte("v1")}, {key: []byte("key_3"), tomb: true}})
	g2 := withOverflow(t, g1, []ovItem{{key: []byte("n1"), val: []byte("v1")}, {key: []byte("key_3"), tomb: true}, {key: []byte("n2"), val: []byte("v2")}})
	g3 := buildMode(t, 805, CompileOptions{}) // a "compaction": unrelated main blob
	for name, c := range map[string]struct{ old, nw []byte }{"first overflow": {base, g1}, "grow overflow": {g1, g2}} {
		p := makeTailPatch(c.old, c.nw)
		got, err := apply(c.old, p)
		if err != nil || !bytes.Equal(got, c.nw) {
			t.Fatalf("%s: %v", name, err)
		}
		if len(p) > len(c.nw)/4 {
			t.Fatalf("%s: patch %d B for a %d B file", name, len(p), len(c.nw))
		}
		for i := 0; i < len(p); i += 3 {
			d := append([]byte(nil), p...)
			d[i] ^= 0x10
			if _, err := apply(c.old, d); err == nil {
				t.Fatalf("%s: byte flip at %d accepted", name, i)
			}
		}
		if _, err := apply(g3, p); err == nil {
			t.Fatalf("%s: applied to an unrelated base", name)
		}
	}
}

// Overflow grows through three tail patches delivered by the real Synchronizer while readers run.
func TestOverflowGrowsUnderConcurrentReads(t *testing.T) {
	const n = 3000
	base := buildMode(t, n, CompileOptions{}) // v2: key_i -> val_i
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
			}
		}(g)
	}
	items := []ovItem{}
	cur := base
	for gen := 1; gen <= 3; gen++ {
		items = append(items, ovItem{key: []byte(fmt.Sprintf("ins_%d", gen)), val: []byte("x")})
		if gen == 1 {
			items = append(items, ovItem{key: []byte("key_1"), val: []byte("edited")})
		}
		if gen == 2 {
			items = append(items, ovItem{key: []byte("key_2"), tomb: true})
		}
		cp := append([]ovItem(nil), items...)
		nw := withOverflow(t, base, cp)
		d.publishWith(nw, makeTailPatch)
		if err := s.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
		cur = nw
	}
	close(stop)
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d bad reads of %d", bad.Load(), reads.Load())
	}
	if a, fb, _ := s.DeltaStats(); a != 3 || fb != 0 {
		t.Fatalf("applied=%d fallbacks=%d", a, fb)
	}
	wantValue(t, s, "key_1", "edited")
	wantValue(t, s, "ins_3", "x")
	if _, err := s.Get([]byte("key_2")); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("tombstoned key_2: %v", err)
	}
	if got, _ := os.ReadFile(s.livePath()); !bytes.Equal(got, cur) {
		t.Fatal("live file differs from the published blob")
	}
	t.Logf("%d reads across 3 overflow generations, 0 bad", reads.Load())
}

// Cross-SDK conformance on the fixtures written by the cloud compiler (testdata/golden/MANIFEST.md): the
// SHA-256s pin the exact bytes all three SDKs must read identically.
func TestOverflowGolden(t *testing.T) {
	dir := os.Getenv("GOLDEN_DIR")
	if dir == "" {
		dir = "../testdata/golden"
	}
	if _, err := os.Stat(filepath.Join(dir, "overflow_v2_base.attos")); err != nil {
		t.Skip("overflow golden fixtures not found (set GOLDEN_DIR)")
	}
	read := func(name, sha string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Skipf("%s: %v", name, err)
		}
		if s := sha256.Sum256(b); fmt.Sprintf("%x", s) != sha {
			t.Fatalf("%s changed: %x", name, s)
		}
		return b
	}
	base := read("overflow_v2_base.attos", "aeced07c6b1385cbaa1dcbdabf66bb346040de0ee769377c17eb6e4e54dabf84")
	gen1 := read("overflow_v2_gen1.attos", "cf576f5dc6b67d075f3878945c1460a848a30ca3a55241cc1e850785c1f0bbad")
	patch := read("overflow_v2_gen1.tailpatch", "35a70422f414bbca42aa9a556e803893f0d51030f4609d7ac885cf5647c8d26b")

	m := openBlob(t, gen1)
	want := map[string]string{"key_0": "val_0", "key_5": "overwritten", "inserted_a": "A", "inserted_b": "B", "key_499": "val_499"}
	for k, v := range want {
		if got, err := m.GetCopy([]byte(k), nil); err != nil || string(got) != v {
			t.Fatalf("%s = %q, %v; want %q", k, got, err, v)
		}
	}
	for _, k := range []string{"key_7", "key_8", "nope"} {
		if _, err := m.Get([]byte(k)); !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("%s must be absent: %v", k, err)
		}
	}
	if m.Len() != 500 { // 500 + 2 inserted - 2 deleted
		t.Fatalf("Len %d", m.Len())
	}
	if got, err := apply(base, patch); err != nil || !bytes.Equal(got, gen1) {
		t.Fatalf("the compiler's tail patch does not rebuild gen1 in the SDK applier: %v", err)
	}
	if m := openBlob(t, base); m.Len() != 500 {
		t.Fatal("base map")
	}
}
