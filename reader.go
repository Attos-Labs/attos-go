package attos

import (
	"encoding/binary"
	"fmt"
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
}

func (m *Map) find(keyHash uint64) (uint64, uint32, bool) {
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
	keyHash := xxhash.Sum64String(key)
	
	offset, length, ok := m.find(keyHash)
	if !ok {
		return "", fmt.Errorf("key not found")
	}
	
	// Because minimal perfect hashing is only perfectly collision-free for KNOWN keys,
	// we technically don't verify the signature here for absolute max speed.
	// We rely on the caller sending a valid key, or we can check the payload.
	
	start := m.blobOffset + int(offset)
	end := start + int(length)
	
	if end > len(m.data) || start > end {
		return "", fmt.Errorf("corrupt payload index")
	}

	val := m.data[start:end]
	if len(val) == 0 {
		return "", nil
	}
	return unsafe.String(unsafe.SliceData(val), len(val)), nil
}

// Get performs an O(1) lookup returning a byte slice.
func (m *Map) Get(key []byte) ([]byte, error) {
	keyHash := xxhash.Sum64(key)
	
	offset, length, ok := m.find(keyHash)
	if !ok {
		return nil, fmt.Errorf("key not found")
	}
	
	start := m.blobOffset + int(offset)
	end := start + int(length)
	
	if end > len(m.data) || start > end {
		return nil, fmt.Errorf("corrupt payload index")
	}

	return m.data[start:end], nil
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

