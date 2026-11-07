package attos

// LSM-lite overflow block (attos-cloud/docs/OVERFLOW_BLOCK.md). A v2 file with flags bit 2 carries a small
// sorted side table between the main blob and the CRC trailer. Lookups check it before the PTHash table;
// the main section of the file is untouched, so a patch only has to rewrite the tail.
//
//	... main blob (ends at baseEnd)
//	hashes   K*8      u64 LE, ascending, XXH64(key, 0)
//	offsets  (K+1)*8  u64 LE, relative to the start of data
//	data     per entry: u16 BE keyLen | u32 BE valLen | key | value   (valLen 0xFFFFFFFF = tombstone, no value)
//	footer   20 B     u64 BE K | u64 BE baseEnd | "OVFL"
//	crc32    4 B      over everything before it
//
// All overflow state is parsed once at open and stored in the immutable mapping; a file without the flag
// has ovK == 0 and pays one predictable branch per lookup.

import (
	"encoding/binary"
	"fmt"

	"github.com/cespare/xxhash/v2"
)

const (
	flagOverflow   = 4
	overflowFooter = 20
	overflowMagic  = "OVFL"
	tombstoneLen   = 0xFFFFFFFF
)

// parseOverflow validates the overflow footer and region of a v2 file whose main blob ends at baseEnd
// and records where the arrays are.
func (mp *mapping) parseOverflow(baseEnd uint64) error {
	data := mp.data
	sz := uint64(len(data))
	if sz < overflowFooter+4 || baseEnd > sz-4-overflowFooter {
		return fmt.Errorf("%w: overflow footer does not fit", ErrCorruptedBinary)
	}
	f := data[sz-4-overflowFooter : sz-4]
	if string(f[16:]) != overflowMagic {
		return fmt.Errorf("%w: missing overflow footer", ErrCorruptedBinary)
	}
	k := binary.BigEndian.Uint64(f)
	if binary.BigEndian.Uint64(f[8:]) != baseEnd {
		return fmt.Errorf("%w: overflow baseEnd does not match the main blob", ErrCorruptedBinary)
	}
	region := sz - 4 - overflowFooter - baseEnd
	if k == 0 || k > region/16 {
		return fmt.Errorf("%w: invalid overflow count %d", ErrCorruptedBinary, k)
	}
	hashOff := baseEnd
	offOff := hashOff + k*8
	dataOff := offOff + (k+1)*8
	if dataOff > sz-4-overflowFooter {
		return fmt.Errorf("%w: overflow arrays exceed the file", ErrCorruptedBinary)
	}
	dataLen := sz - 4 - overflowFooter - dataOff
	le := binary.LittleEndian
	var prev uint64
	for i := uint64(0); i < k; i++ {
		h := le.Uint64(data[hashOff+i*8:])
		if i > 0 && h < prev {
			return fmt.Errorf("%w: overflow hashes not sorted", ErrCorruptedBinary)
		}
		prev = h
	}
	var po uint64
	for i := uint64(0); i <= k; i++ {
		o := le.Uint64(data[offOff+i*8:])
		if (i == 0 && o != 0) || o < po || o > dataLen {
			return fmt.Errorf("%w: invalid overflow offsets", ErrCorruptedBinary)
		}
		po = o
	}
	if po != dataLen {
		return fmt.Errorf("%w: overflow data length mismatch", ErrCorruptedBinary)
	}
	mp.ovK, mp.ovHashOff, mp.ovOffOff, mp.ovDataOff = int(k), int(hashOff), int(offOff), int(dataOff)
	// Reject filter: 32 bits per key rounded up to a power of two, one probe per lookup. It is built here, once
	// per mapping (a few KB), so most lookups of main-table keys cost one cache line instead of a binary search.
	bits := uint64(64)
	for bits < k*32 {
		bits <<= 1
	}
	mp.ovFilter = make([]uint64, bits/64)
	mp.ovMask = bits - 1
	for i := uint64(0); i < k; i++ {
		x := (le.Uint64(data[hashOff+i*8:]) >> 20) & mp.ovMask
		mp.ovFilter[x>>6] |= 1 << (x & 63)
	}
	return nil
}

// ovEntry returns the record of overflow entry i: key, value, tombstone, ok (false = corrupt entry).
func (mp *mapping) ovEntry(i int) (key, val []byte, tomb, ok bool) {
	le := binary.LittleEndian
	s := mp.ovDataOff + int(le.Uint64(mp.data[mp.ovOffOff+i*8:]))
	e := mp.ovDataOff + int(le.Uint64(mp.data[mp.ovOffOff+(i+1)*8:]))
	if e-s < 6 {
		return nil, nil, false, false
	}
	rec := mp.data[s:e]
	kl := int(binary.BigEndian.Uint16(rec))
	vl := binary.BigEndian.Uint32(rec[2:])
	if vl == tombstoneLen {
		if 6+kl != len(rec) {
			return nil, nil, false, false
		}
		return rec[6 : 6+kl], nil, true, true
	}
	if 6+kl+int(vl) != len(rec) {
		return nil, nil, false, false
	}
	return rec[6 : 6+kl], rec[6+kl:], false, true
}

// overflowLookup searches the overflow for key (hash h = XXH64(key)). found reports that the overflow
// decides the answer: a value, or ErrKeyNotFound for a tombstone. No allocation.
func (mp *mapping) overflowLookup(h uint64, key []byte) (v []byte, found bool, err error) {
	x := (h >> 20) & mp.ovMask
	if mp.ovFilter[x>>6]&(1<<(x&63)) == 0 {
		return nil, false, nil
	}
	d := mp.data
	lo, hi := 0, mp.ovK
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if binary.LittleEndian.Uint64(d[mp.ovHashOff+mid*8:]) < h {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	for i := lo; i < mp.ovK && binary.LittleEndian.Uint64(d[mp.ovHashOff+i*8:]) == h; i++ {
		k, val, tomb, ok := mp.ovEntry(i)
		if !ok {
			return nil, true, ErrCorruptedBinary
		}
		if string(k) != string(key) { // equal hash, different key: keep looking, then the main table
			continue
		}
		if tomb {
			return nil, true, ErrKeyNotFound
		}
		return val, true, nil
	}
	return nil, false, nil
}

// overflowHas reports whether the overflow holds any entry (value or tombstone) for key.
func (mp *mapping) overflowHas(key []byte, h uint64) bool {
	_, found, _ := mp.overflowLookup(h, key)
	return found
}

// countRowsOverflow is the number of live keys: main keys, plus overflow keys that are new, minus
// tombstoned main keys.
func (mp *mapping) countRowsOverflow() int {
	n := int(mp.totalKeys)
	for i := 0; i < mp.ovK; i++ {
		k, _, tomb, ok := mp.ovEntry(i)
		if !ok {
			continue
		}
		_, err := mp.entryMain(xxhash.Sum64(k), k, nil)
		inMain := err == nil
		switch {
		case tomb && inMain:
			n--
		case !tomb && !inMain:
			n++
		}
	}
	return n
}

// scanV2Overflow is scanV2 for a file that may carry an overflow block: main entries that the overflow
// overrides or deletes are skipped, then the live overflow entries follow. (In lean-key files the main
// records carry fingerprints rather than keys, so they cannot be masked; Scan already reports
// fingerprints for them.)
func (mp *mapping) scanV2Overflow(fn func(key, value []byte) bool) {
	if mp.ovK == 0 {
		mp.scanV2(fn)
		return
	}
	stopped := false
	mp.scanV2(func(k, v []byte) bool {
		if !mp.lean && mp.overflowHas(k, xxhash.Sum64(k)) {
			return true
		}
		if !fn(k, v) {
			stopped = true
			return false
		}
		return true
	})
	if stopped {
		return
	}
	for i := 0; i < mp.ovK; i++ {
		if k, v, tomb, ok := mp.ovEntry(i); ok && !tomb && !fn(k, v) {
			return
		}
	}
}
