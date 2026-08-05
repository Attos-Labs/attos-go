package attos

import (
	"encoding/binary"
	"sync/atomic"
	"unsafe"

	"github.com/cespare/xxhash/v2"
)

type Map struct {
	data []byte // full file mmap'd read-only

	// Header metadata
	version    uint32
	is32Bit    bool
	totalKeys  uint64
	numBuckets uint64
	globalSeed uint64

	// Offsets
	routingOffset int
	valuesOffset  int
	blobOffset    int

	closed atomic.Bool
	pinned bool
}

func (m *Map) find(keyHash uint64) (uint64, uint32, bool) {
	if m.numBuckets == 0 || m.totalKeys == 0 {
		return 0, 0, false
	}
	high := uint32(keyHash >> 32)
	low := uint32(keyHash)

	segmentIdx := uint64(high) % m.numBuckets

	var d uint32
	if m.is32Bit {
		dispOff := m.routingOffset + int(segmentIdx*4)
		d = binary.BigEndian.Uint32(m.data[dispOff:])
	} else {
		dispOff := m.routingOffset + int(segmentIdx*2)
		d = uint32(binary.BigEndian.Uint16(m.data[dispOff:]))
	}

	finalIndex := (uint64(low) ^ uint64(d)) % m.totalKeys

	valueOff := m.valuesOffset + int(finalIndex*8)
	packed := binary.BigEndian.Uint64(m.data[valueOff:])

	offset := uint32(packed >> 32)
	length := uint32(packed)

	return uint64(offset), length, true
}

// GetString performs an O(1) lookup returning a string.
// Zero-allocation: memory is mmap'd read-only, safe to alias.
func (m *Map) GetString(key string) (string, error) {
	if m.closed.Load() {
		return "", ErrMapClosed
	}

	keyHash := xxhash.Sum64String(key)

	offset, length, ok := m.find(keyHash)
	if !ok || length == 0 {
		return "", ErrKeyNotFound
	}

	start := m.blobOffset + int(offset)
	end := start + int(length)

	if end > len(m.data) || start > end {
		return "", ErrCorruptedBinary
	}

	val := m.data[start:end]
	if len(val) == 0 {
		return "", nil
	}
	// Verify actual key match to handle false positives
	if len(val) < 2 {
		return "", ErrCorruptedBinary
	}
	keyLen := int(binary.BigEndian.Uint16(val[:2]))
	if len(val) < 2+keyLen {
		return "", ErrCorruptedBinary
	}
	keyBytes := val[2 : 2+keyLen]
	if string(keyBytes) != key {
		return "", ErrKeyNotFound
	}

	payload := val[2+keyLen:]
	return unsafe.String(unsafe.SliceData(payload), len(payload)), nil
}

// Get performs an O(1) lookup returning a byte slice.
func (m *Map) Get(key []byte) ([]byte, error) {
	if m.closed.Load() {
		return nil, ErrMapClosed
	}

	keyHash := xxhash.Sum64(key)

	offset, length, ok := m.find(keyHash)
	if !ok || length == 0 {
		return nil, ErrKeyNotFound
	}

	start := m.blobOffset + int(offset)
	end := start + int(length)

	if end > len(m.data) || start > end {
		return nil, ErrCorruptedBinary
	}

	val := m.data[start:end]
	// Verify actual key match
	if len(val) < 2 {
		return nil, ErrCorruptedBinary
	}
	keyLen := int(binary.BigEndian.Uint16(val[:2]))
	if len(val) < 2+keyLen {
		return nil, ErrCorruptedBinary
	}
	keyBytes := val[2 : 2+keyLen]

	// Check equality
	if len(keyBytes) != len(key) {
		return nil, ErrKeyNotFound
	}
	for i := range keyBytes {
		if keyBytes[i] != key[i] {
			return nil, ErrKeyNotFound
		}
	}

	return val[2+keyLen:], nil
}

// Scan iterates sequentially over all entries in the blob.
// Note that order is arbitrary (dictated by the MPHF construction).
func (m *Map) Scan(fn func(key, value []byte) bool) {
	if m.closed.Load() || m.totalKeys == 0 {
		return
	}

	for i := uint64(0); i < m.totalKeys; i++ {
		valueOff := m.valuesOffset + int(i*8)
		packed := binary.BigEndian.Uint64(m.data[valueOff:])

		offset := uint32(packed >> 32)
		length := uint32(packed)
		if length == 0 {
			continue
		}

		start := m.blobOffset + int(offset)
		end := start + int(length)

		if end <= len(m.data) && start <= end {
			val := m.data[start:end]
			if len(val) >= 2 {
				keyLen := int(binary.BigEndian.Uint16(val[:2]))
				if len(val) >= 2+keyLen {
					keyBytes := val[2 : 2+keyLen]
					valueBytes := val[2+keyLen:]
					if !fn(keyBytes, valueBytes) {
						break
					}
				}
			}
		}
	}
}

// Reload opens a new file, validates it, and replaces the current map's data.
// NOTE: For thread-safety, callers should use Synchronizer or atomic.Pointer externally.
func (m *Map) Reload(path string) error {
	newMap, err := OpenWithConfig(path, OpenConfig{
		PinToMemory: m.pinned,
		// Maintain whatever state is appropriate; for SDK simplicity we just use default
	})
	if err != nil {
		return err
	}

	// Swap contents
	oldData := m.data
	m.data = newMap.data
	m.version = newMap.version
	m.is32Bit = newMap.is32Bit
	m.totalKeys = newMap.totalKeys
	m.numBuckets = newMap.numBuckets
	m.globalSeed = newMap.globalSeed
	m.routingOffset = newMap.routingOffset
	m.valuesOffset = newMap.valuesOffset
	m.blobOffset = newMap.blobOffset

	// Now we need to manually close the old data, since m now holds the new data
	oldMap := &Map{
		data:   oldData,
		pinned: m.pinned,
	}
	_ = oldMap.Close() // unmaps and munlocks old data

	return nil
}

func warmUp(data []byte) {
	const pageSize = 4096
	var dummy byte
	for i := 0; i < len(data); i += pageSize {
		dummy += data[i]
	}
	if len(data) > 0 {
		dummy += data[len(data)-1]
	}
}
