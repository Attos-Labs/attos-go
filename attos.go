package attos

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"log"
	"math"
	"os"
	"time"

	"github.com/Attos-Labs/attos-go/internal/mmap"
)

const magicATTO = "ATTO"

// headerSize is the fixed v1 header length (CONTRACTS.md §1.1).
const headerSize = 33

// OpenConfig controls how a Map is opened and validated.
// All fields are optional; zero values select safe defaults.
type OpenConfig struct {
	// PinToMemory calls mlock(2) after mapping to prevent the OS from
	// paging out the region. Recommended for latency-critical deployments
	// (GKE Standard supports mlock; Autopilot does not).
	// Default: false. Failure is non-fatal; a warning is emitted via Logger.
	PinToMemory bool

	// ValidateChecksum verifies the CRC32-IEEE trailer before the map is
	// exposed. The Synchronizer always validates downloaded blobs regardless
	// of this flag. Default: false (direct Open only).
	ValidateChecksum bool

	// SkipWarmUp disables the sequential page warm-up performed after mapping.
	// Default: false (warm-up enabled).
	SkipWarmUp bool

	// RetireGrace is the minimum time a replaced map stays mapped after a hot
	// swap (in addition to waiting for in-flight lookups to finish). It bounds
	// the validity window of zero-copy values returned by Get/GetString: such
	// a value stays readable for at least RetireGrace after the map it came
	// from was replaced. Default (0): DefaultRetireGrace. Negative: no grace.
	RetireGrace time.Duration

	// Logger is used for non-fatal operational messages (mlock failures,
	// warm-up, etc.). If nil, log.Default() is used.
	Logger *log.Logger

	// LocalObjectStore instructs the Synchronizer to pull directly from a
	// customer S3 bucket instead of polling the Attos control plane.
	LocalObjectStore bool

	// LocalS3Bucket is the name of the customer's S3 bucket to pull from
	// when LocalObjectStore is true. The object key is "<datasetID>.attos".
	LocalS3Bucket string
}

// DefaultRetireGrace is the default zero-copy validity window after a swap.
const DefaultRetireGrace = 2 * time.Second

func (cfg OpenConfig) logger() *log.Logger {
	if cfg.Logger != nil {
		return cfg.Logger
	}
	return log.Default()
}

func (cfg OpenConfig) retireGrace() time.Duration {
	switch {
	case cfg.RetireGrace == 0:
		return DefaultRetireGrace
	case cfg.RetireGrace < 0:
		return 0
	}
	return cfg.RetireGrace
}

// Open maps path using default OpenConfig (no checksum, no mlock).
func Open(path string) (*Map, error) {
	return OpenWithConfig(path, OpenConfig{})
}

// OpenWithConfig maps path read-only, validates it and returns a Map.
// Every failure mode returns a typed error: ErrInvalidMagic,
// ErrInvalidVersion (*VersionError), ErrCorruptedBinary or ErrChecksumMismatch.
func OpenWithConfig(path string, cfg OpenConfig) (*Map, error) {
	mp, err := openMapping(path, cfg, cfg.ValidateChecksum)
	if err != nil {
		return nil, err
	}
	m := &Map{cfg: cfg}
	m.cur.Store(mp)
	return m, nil
}

// ValidateFile fully validates a blob on disk (magic, version, flag, size
// arithmetic and CRC32) without keeping it mapped. This is the validation the
// sync path applies before publishing a downloaded blob.
func ValidateFile(path string) error {
	mp, err := openMapping(path, OpenConfig{SkipWarmUp: true}, true)
	if err != nil {
		return err
	}
	return mp.unmap()
}

// openMapping maps path and validates it. With checkCRC the CRC32 trailer is
// verified too. The returned mapping owns the region; the file descriptor is
// closed before returning (the mapping keeps the inode alive).
func openMapping(path string, cfg OpenConfig, checkCRC bool) (*mapping, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	f.Close()
	if err != nil {
		return nil, err
	}
	if info.Size() < 4 {
		return nil, fmt.Errorf("%w: file too small (%d bytes)", ErrCorruptedBinary, info.Size())
	}
	data, err := mmap.MapFile(path)
	if err != nil {
		return nil, fmt.Errorf("mmap: %w", err)
	}
	mp, err := parseHeader(data, checkCRC)
	if err != nil {
		mmap.Unmap(data)
		return nil, err
	}
	if cfg.PinToMemory {
		if err := mmap.Mlock(data); err != nil {
			cfg.logger().Printf("[attos] Warning: mlock failed (non-fatal): %v", err)
		} else {
			mp.pinned = true
		}
	}
	if !cfg.SkipWarmUp {
		warmUp(data)
	}
	return mp, nil
}

// parseHeader validates a v1 blob per CONTRACTS.md §1.1 / §1.6.
func parseHeader(data []byte, checkCRC bool) (*mapping, error) {
	sz := uint64(len(data))
	if sz < 4 {
		return nil, fmt.Errorf("%w: file too small (%d bytes)", ErrCorruptedBinary, sz)
	}
	if string(data[:4]) != magicATTO {
		return nil, ErrInvalidMagic
	}
	if sz < 8 {
		return nil, fmt.Errorf("%w: file too small for version (%d bytes)", ErrCorruptedBinary, sz)
	}
	be := binary.BigEndian
	if v := be.Uint32(data[4:8]); v != 1 {
		return nil, &VersionError{Version: v}
	}
	if sz < headerSize {
		return nil, fmt.Errorf("%w: file too small for header (%d bytes)", ErrCorruptedBinary, sz)
	}
	flag := data[8]
	var w uint64
	switch flag {
	case 0x01:
		w = 2
	case 0x02:
		w = 4
	default:
		return nil, fmt.Errorf("%w: invalid routing flag 0x%02x", ErrCorruptedBinary, flag)
	}
	S := be.Uint64(data[9:17])
	B := be.Uint64(data[17:25])
	if S == 0 || B == 0 {
		return nil, fmt.Errorf("%w: zero slots (%d) or buckets (%d)", ErrCorruptedBinary, S, B)
	}
	// Overflow-safe size arithmetic: D + 4 <= size.
	if B > (math.MaxUint64-headerSize)/w || S > math.MaxUint64/8 {
		return nil, fmt.Errorf("%w: header counts overflow", ErrCorruptedBinary)
	}
	R := headerSize + B*w
	if R > math.MaxUint64-S*8-4 {
		return nil, fmt.Errorf("%w: header counts overflow", ErrCorruptedBinary)
	}
	D := R + S*8
	if D+4 > sz {
		return nil, fmt.Errorf("%w: expected at least %d bytes, got %d", ErrCorruptedBinary, D+4, sz)
	}
	if checkCRC {
		end := sz - 4
		want := be.Uint32(data[end:])
		if got := crc32.ChecksumIEEE(data[:end]); got != want {
			return nil, fmt.Errorf("%w: trailer 0x%08x, computed 0x%08x", ErrChecksumMismatch, want, got)
		}
	}
	return &mapping{
		data:    data,
		is32:    flag == 0x02,
		slots:   S,
		buckets: B,
		r:       int(R),
		d:       int(D),
		end:     int(sz - 4),
		size:    int64(sz),
	}, nil
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
