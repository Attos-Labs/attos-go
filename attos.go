package attos

import (
	"encoding/binary"
	"fmt"
	"os"

	binInternal "github.com/attos/sdk/internal/binary"
	"github.com/attos/sdk/internal/mmap"
)

// File format constants
const (
	dbKeysOnly       = 1 << iota
	magicZeroLatency = "MPHB"
	magicLegacy      = "MPHC"
)

// Open memory-maps the entire .nh file and deserializes the map index
// directly from the mmap'd bytes — zero copies, zero disk I/O on read.
func Open(path string) (*Map, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	sz := info.Size()
	if sz < 64+32 {
		return nil, fmt.Errorf("%s: file too small (%d bytes)", path, sz)
	}

	data, err := mmap.MapFile(path)
	if err != nil {
		return nil, fmt.Errorf("mmap: %w", err)
	}

	m := &Map{data: data}
	be := binary.BigEndian
	magic := string(data[:4])
	if magic != magicZeroLatency && magic != magicLegacy {
		mmap.Unmap(data)
		return nil, fmt.Errorf("bad magic: %q", magic)
	}
	if magic == magicLegacy {
		mmap.Unmap(data)
		return nil, fmt.Errorf("legacy format not supported in zero-copy mode")
	}

	m.flags = be.Uint32(data[4:8])
	m.nkeys = be.Uint64(data[24:32])
	offtbl := be.Uint64(data[32:40])

	if offtbl < 64 || offtbl >= uint64(sz)-32 {
		mmap.Unmap(data)
		return nil, fmt.Errorf("corrupt header: offtbl=%d, size=%d", offtbl, sz)
	}

	meta := data[offtbl : uint64(sz)-32]
	offsz := (m.nkeys + 1) * 8
	m.offset = binInternal.BytesToUint64Slice(meta[:offsz])
	meta = meta[offsz:]

	metaConsumed := offsz
	fileOff := offtbl + metaConsumed
	if rem := fileOff % 8; rem != 0 {
		skip := 8 - rem
		if skip <= uint64(len(meta)) {
			meta = meta[skip:]
		}
	}

	if err := m.deserializeMap(meta); err != nil {
		mmap.Unmap(data)
		return nil, fmt.Errorf("deserialize map: %w", err)
	}

	warmUp(data)
	return m, nil
}

// Close unmaps the database.
func (m *Map) Close() error {
	if m.data != nil {
		err := mmap.Unmap(m.data)
		m.data = nil
		return err
	}
	return nil
}
