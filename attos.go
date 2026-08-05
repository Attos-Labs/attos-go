package attos

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"log"
	"os"

	"github.com/Attos-Labs/attos-go/internal/mmap"
)

const magicATTO = "ATTO"

type OpenConfig struct {
	PinToMemory      bool
	ValidateChecksum bool
}

func Open(path string) (*Map, error) {
	return OpenWithConfig(path, OpenConfig{})
}

func OpenWithConfig(path string, cfg OpenConfig) (*Map, error) {
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
	if sz < 33 { // Min header size for ATTO
		return nil, fmt.Errorf("%w: file too small (%d bytes)", ErrCorruptedBinary, sz)
	}

	data, err := mmap.MapFile(path)
	if err != nil {
		return nil, fmt.Errorf("mmap: %w", err)
	}

	m := &Map{data: data}
	magic := string(data[:4])
	if magic != magicATTO {
		mmap.Unmap(data)
		return nil, ErrInvalidMagic
	}

	be := binary.BigEndian
	m.version = be.Uint32(data[4:8])
	if m.version != 1 {
		mmap.Unmap(data)
		return nil, ErrInvalidVersion
	}

	flag := data[8]
	m.is32Bit = (flag == 0x02)
	m.totalKeys = be.Uint64(data[9:17])
	m.numBuckets = be.Uint64(data[17:25])
	m.globalSeed = be.Uint64(data[25:33])

	m.routingOffset = 33
	routingByteSize := m.numBuckets * 2
	if m.is32Bit {
		routingByteSize = m.numBuckets * 4
	}

	m.valuesOffset = int(m.routingOffset) + int(routingByteSize)
	m.blobOffset = m.valuesOffset + int(m.totalKeys*8)

	if m.blobOffset > int(sz) {
		mmap.Unmap(data)
		return nil, fmt.Errorf("%w: expected min size %d, got %d", ErrCorruptedBinary, m.blobOffset, sz)
	}

	if cfg.ValidateChecksum && len(data) >= m.blobOffset+4 {
		// Assuming the last 4 bytes of the file are the CRC32
		dataLen := len(data) - 4
		expectedCRC := be.Uint32(data[dataLen:])
		actualCRC := crc32.ChecksumIEEE(data[:dataLen])
		if expectedCRC != actualCRC {
			mmap.Unmap(data)
			return nil, ErrChecksumMismatch
		}
	}

	if cfg.PinToMemory {
		if err := mmap.Mlock(data); err != nil {
			log.Printf("Warning: failed to mlock memory: %v", err)
			// Return ErrMlockFailed if you want it to be fatal, or log and continue.
			// The instructions said "log warning if fails".
		} else {
			m.pinned = true
		}
	}

	warmUp(data)
	return m, nil
}

func (m *Map) Close() error {
	m.closed.Store(true)
	if m.data != nil {
		if m.pinned {
			_ = mmap.Munlock(m.data)
		}
		err := mmap.Unmap(m.data)
		return err
	}
	return nil
}
