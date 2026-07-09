package attos

import (
	binarygo "encoding/binary"
	"fmt"
	"math/bits"
	"runtime"
	"unsafe"

	"github.com/Attos-Labs/attos-go/internal/binary"
	"github.com/Attos-Labs/attos-go/internal/hash"
)

// ─────────────────────────────────────────────────────────────
// Map holds the entire data blob in a single mmap region.
// All lookups are pure pointer arithmetic — zero syscalls,
// zero allocations, zero locks.
// ─────────────────────────────────────────────────────────────

// Map represents a static key-value store optimized for zero-latency lookups.
type Map struct {
	data []byte // full file mmap'd read-only

	// State (deserialized from mmap)
	mapSalt  uint64
	mapBits  []RoutingTable // bitvectors per level
	mapRanks []uint64       // pre-computed cumulative ranks

	// Offset table (slice into mmap — no copy)
	offset []uint64 // [i]=uint32 signature | uint32 file offset
	nkeys  uint64
	flags  uint32
}

// RoutingTable is a read-only bitvector backed by mmap'd memory.
// rankIdx provides O(1) rank queries via a pre-computed prefix-sum.
type RoutingTable struct {
	v       []uint64 // bitvector words (from mmap)
	rankIdx []uint32 // rankIdx[i] = popcount of words 0..(i*4)-1
}

func (r *RoutingTable) size() uint64 {
	return uint64(len(r.v)) * 64
}

func (r *RoutingTable) isSet(i uint64) bool {
	return 1 == (1 & (r.v[i/64] >> (i % 64)))
}

// rank returns the number of set bits before position i.
func (r *RoutingTable) rank(i uint64) uint64 {
	wordIdx := i / 64
	blockIdx := wordIdx / 4

	res := uint64(r.rankIdx[blockIdx])

	startWord := blockIdx * 4
	for j := startWord; j < wordIdx; j++ {
		res += uint64(bits.OnesCount64(r.v[j]))
	}

	y := i % 64
	mask := (uint64(1) << y) - 1
	res += uint64(bits.OnesCount64(r.v[wordIdx] & mask))

	return res
}

func (r *RoutingTable) buildRankIndex() {
	n := len(r.v)
	idxLen := (n+3)/4 + 1
	r.rankIdx = make([]uint32, idxLen)
	var cumulative uint64
	for i := 0; i < n; i++ {
		if i%4 == 0 {
			r.rankIdx[i/4] = uint32(cumulative)
		}
		cumulative += uint64(bits.OnesCount64(r.v[i]))
	}
	r.rankIdx[idxLen-1] = uint32(cumulative)
}

func (r *RoutingTable) popcount() uint64 {
	if len(r.rankIdx) > 0 {
		return uint64(r.rankIdx[len(r.rankIdx)-1])
	}
	var p uint64
	for _, w := range r.v {
		p += uint64(bits.OnesCount64(w))
	}
	return p
}

// find performs the zero-latency lookup — pure arithmetic on mmap'd bitvectors.
func (m *Map) find(key uint64) (uint64, bool) {
	for lvl, bv := range m.mapBits {
		i := hash.Block(key, m.mapSalt, uint32(lvl)) % bv.size()
		if !bv.isSet(i) {
			continue
		}
		rank := 1 + m.mapRanks[lvl] + bv.rank(i)
		return rank - 1, true
	}
	return 0, false
}

func (m *Map) get(keyHash uint64) ([]byte, bool) {
	idx, ok := m.find(keyHash)
	if !ok {
		return nil, false
	}

	if idx+1 >= uint64(len(m.offset)) {
		return nil, false
	}

	entry := m.offset[idx]
	sig := uint32(entry >> 32)
	valOff := uint32(entry)

	expectedSig := uint32(keyHash >> 32)
	if sig != expectedSig {
		return nil, false
	}

	nextEntry := m.offset[idx+1]
	nextValOff := uint32(nextEntry)

	valStart := uint64(valOff)
	valEnd := uint64(nextValOff)

	if valEnd > uint64(len(m.data)) || valStart > valEnd {
		return nil, false
	}

	val := m.data[valStart:valEnd]
	return val, true
}

// GetString performs an O(1) lookup returning a string.
// Zero-allocation: memory is mmap'd read-only, safe to alias.
func (m *Map) GetString(key string) (string, error) {
	val, ok := m.get(hash.String(key))
	if !ok {
		return "", fmt.Errorf("key not found")
	}
	if len(val) == 0 {
		return "", nil
	}
	return unsafe.String(unsafe.SliceData(val), len(val)), nil
}

// Get performs an O(1) lookup returning a byte slice.
func (m *Map) Get(key []byte) ([]byte, error) {
	val, ok := m.get(hash.Bytes(key))
	if !ok {
		return nil, fmt.Errorf("key not found")
	}
	return val, nil
}

// deserializeMap reads the header and bitvectors directly from mmap'd bytes.
func (m *Map) deserializeMap(buf []byte) error {
	if len(buf) < 16 {
		return fmt.Errorf("header too short: %d bytes", len(buf))
	}

	le := binarygo.LittleEndian
	ver := buf[0]
	if ver != 1 {
		return fmt.Errorf("unsupported version: %d", ver)
	}

	nlevels := le.Uint32(buf[4:8])
	m.mapSalt = le.Uint64(buf[8:16])

	if nlevels == 0 || nlevels > 4000 {
		return fmt.Errorf("invalid levels: %d", nlevels)
	}

	buf = buf[16:]
	m.mapBits = make([]RoutingTable, nlevels)

	for i := uint32(0); i < nlevels; i++ {
		if len(buf) < 8 {
			return fmt.Errorf("level %d: truncated", i)
		}
		bvWords := le.Uint64(buf[:8])
		if bvWords == 0 || bvWords > (1<<32) {
			return fmt.Errorf("level %d: invalid bitvector length %d", i, bvWords)
		}

		byteLen := bvWords * 8
		if uint64(len(buf)-8) < byteLen {
			return fmt.Errorf("level %d: bitvector data truncated", i)
		}

		m.mapBits[i] = RoutingTable{
			v: binary.BytesToUint64Slice(buf[8 : 8+byteLen]),
		}
		m.mapBits[i].buildRankIndex()
		buf = buf[8+byteLen:]
	}

	m.mapRanks = make([]uint64, nlevels)
	var pop uint64
	for l := range m.mapBits {
		m.mapRanks[l] = pop
		pop += m.mapBits[l].popcount()
	}

	return nil
}

// warmUp forces the OS to page-in the entire mmap'd region by
// touching every 4KB page.
func warmUp(data []byte) {
	const pageSize = 4096
	var dummy byte
	for i := 0; i < len(data); i += pageSize {
		dummy += data[i]
	}
	if len(data) > 0 {
		dummy += data[len(data)-1]
	}
	runtime.KeepAlive(dummy)
}
