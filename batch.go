package attos

import (
	"encoding/binary"
	"errors"

	"github.com/cespare/xxhash/v2"
)

// ErrBatchOutTooSmall is returned by GetMany when out is shorter than keys.
var ErrBatchOutTooSmall = errors.New("batch output slice shorter than keys")

// batchGroup is how many keys GetMany resolves per pass.
const batchGroup = 32

// GetMany looks up keys in one call and stores each zero-copy value in
// out[i] (nil when the key is absent). It returns the number of keys found.
// len(out) must be at least len(keys). Value slices have the same validity
// window as Get.
//
// It is cheaper per key than repeated Get on large files: the mapping is
// pinned once, and for each group of keys the routing-table reads are issued
// together, then the offset-table reads, so their cache misses overlap instead
// of stalling one after another. Every key counts as a lookup (absent keys as
// misses); batch calls are not latency-sampled. Zero allocations.
func (m *Map) GetMany(keys [][]byte, out [][]byte) (int, error) {
	if len(out) < len(keys) {
		return 0, ErrBatchOutTooSmall
	}
	if len(keys) == 0 {
		return 0, nil
	}
	mp, s, _ := m.enter(xxhash.Sum64(keys[0]))
	if mp == nil {
		return 0, m.noMapErr()
	}
	defer s.exit.Add(1)
	mp.batchExtra.Add(uint64(len(keys) - 1))
	layered := mp.version == 2 // the pre-read pipeline below assumes the v1 table layout
	var hs, slots [batchGroup]uint64
	found := 0
	for base := 0; base < len(keys); base += batchGroup {
		g := keys[base:min(base+batchGroup, len(keys))]
		for j, k := range g {
			hs[j] = xxhash.Sum64(k)
		}
		if !layered {
			// Pass 1: routing words, independent loads that the CPU overlaps.
			var sink uint64
			for j := range g {
				var d uint64
				bk := uint64(uint32(hs[j]>>32)) % mp.buckets
				if mp.is32 {
					d = uint64(binary.BigEndian.Uint32(mp.data[uint64(headerSize)+bk*4:]))
				} else {
					d = uint64(binary.BigEndian.Uint16(mp.data[uint64(headerSize)+bk*2:]))
				}
				slots[j] = (uint64(uint32(hs[j])) ^ d) % mp.slots
				sink += d
			}
			// Pass 2: offset-table words.
			for j := range g {
				sink += binary.BigEndian.Uint64(mp.data[uint64(mp.r)+slots[j]*8:])
			}
			if sink == 0x9E3779B97F4A7C15 { // practically never; keeps the pre-reads from being optimised away
				s.miss.Add(0)
			}
		}
		for j, k := range g {
			v, err := mp.entry(hs[j], k)
			switch err {
			case nil:
				out[base+j] = v
				found++
			case ErrKeyNotFound:
				out[base+j] = nil
				s.miss.Add(1)
			default:
				return found, err
			}
		}
	}
	return found, nil
}
