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

// OpenConfig controls how a Map is opened and validated.
// All fields are optional; zero values select safe defaults.
type OpenConfig struct {
	// PinToMemory calls mlock(2) after mapping to prevent the OS from
	// paging out the region. Recommended for latency-critical deployments.
	// Default: false. Failure is non-fatal; a warning is emitted via Logger.
	PinToMemory bool

	// ValidateChecksum verifies the CRC32-IEEE trailer appended by Builder.Compile.
	// Enable on startup; disable on the hot path.
	// Default: false.
	ValidateChecksum bool

	// Logger is used for non-fatal operational messages (mlock failures,
	// warm-up, etc.). If nil, log.Default() is used.
	Logger *log.Logger

	// LocalObjectStore instructs the Synchronizer to pull directly from a local
	// S3 bucket instead of polling the Attos control plane.
	LocalObjectStore bool

	// LocalS3Bucket is the name of the customer's S3 bucket to pull from
	// when LocalObjectStore is true.
	LocalS3Bucket string
}

func (cfg OpenConfig) logger() *log.Logger {
	if cfg.Logger != nil {
		return cfg.Logger
	}
	return log.Default()
}

// Open maps path using default OpenConfig (no checksum, no mlock).
func Open(path string) (*Map, error) {
	return OpenWithConfig(path, OpenConfig{})
}

// OpenWithConfig maps path and applies all options in cfg.
// Returns a typed sentinel error on every known failure mode.
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
		// Last 4 bytes of the file are the CRC32-IEEE trailer written by Builder.Compile.
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
			cfg.logger().Printf("[attos] Warning: mlock failed (non-fatal): %v", err)
			// ErrMlockFailed is available for callers that want fatal behaviour.
		} else {
			m.pinned = true
		}
	}

	warmUp(data)
	return m, nil
}

// Close unmunmaps the backing region. It is safe to call multiple times;
// subsequent calls are no-ops. Callers using Synchronizer should call
// Synchronizer.Close() instead — it handles atomic pointer swap and
// goroutine drain.
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

// discardOutput is used for warmUp to prevent the compiler eliding the loop.
var discardOutput byte

func warmUp(data []byte) {
	const pageSize = 4096
	var dummy byte
	for i := 0; i < len(data); i += pageSize {
		dummy += data[i]
	}
	if len(data) > 0 {
		dummy += data[len(data)-1]
	}
	discardOutput = dummy
}


