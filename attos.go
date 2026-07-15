package attos

import (
	"encoding/binary"
	"fmt"
	"os"

	"github.com/Attos-Labs/attos-go/internal/mmap"
)

const magicATTO = "ATTO"

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
	if sz < 33 { // Min header size for ATTO
		return nil, fmt.Errorf("%s: file too small (%d bytes)", path, sz)
	}

	data, err := mmap.MapFile(path)
	if err != nil {
		return nil, fmt.Errorf("mmap: %w", err)
	}

	m := &Map{data: data}
	magic := string(data[:4])
	if magic != magicATTO {
		mmap.Unmap(data)
		return nil, fmt.Errorf("bad magic: %q", magic)
	}

	be := binary.BigEndian
	m.version = be.Uint32(data[4:8])
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
		return nil, fmt.Errorf("corrupt header: expected min size %d, got %d", m.blobOffset, sz)
	}

	warmUp(data)
	return m, nil
}

func (m *Map) Close() error {
	if m.data != nil {
		err := mmap.Unmap(m.data)
		m.data = nil
		return err
	}
	return nil
}

