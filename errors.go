package attos

import (
	"errors"
	"fmt"
)

// Reader / runtime errors (CONTRACTS.md §1.6).
var (
	ErrKeyNotFound     = errors.New("key not found")
	ErrCorruptedBinary = errors.New("corrupted binary")
	ErrInvalidMagic    = errors.New("invalid magic bytes")
	ErrInvalidVersion  = errors.New("invalid version")
	// ErrUnsupportedFeature is returned (via *FeatureError) for a well-formed file that uses a
	// format feature newer than this SDK knows. The fix is to upgrade the SDK.
	ErrUnsupportedFeature = errors.New("unsupported format feature")
	ErrChecksumMismatch   = errors.New("checksum mismatch")
	ErrMapClosed          = errors.New("map closed")
	ErrMlockFailed        = errors.New("mlock failed")
	// ErrNoMap is returned by Synchronizer lookups before the first blob is loaded.
	ErrNoMap = errors.New("no blob loaded")
)

// VersionError is returned when a blob carries an unsupported version.
// It matches ErrInvalidVersion with errors.Is.
type VersionError struct{ Version uint32 }

func (e *VersionError) Error() string {
	return fmt.Sprintf("invalid version: unsupported format version %d (attos-go %s reads versions 1-2); upgrade the Attos SDK", e.Version, Version)
}
func (e *VersionError) Unwrap() error { return ErrInvalidVersion }

// FeatureError is returned when a file passes its CRC but uses a format feature this SDK
// does not implement (an unknown flag bit, offset width or fingerprint width). It matches
// ErrUnsupportedFeature with errors.Is. Fail-closed: the file is never partially read.
type FeatureError struct{ Feature string }

func (e *FeatureError) Error() string {
	return fmt.Sprintf("%v: %s (attos-go %s does not support it); upgrade the Attos SDK", ErrUnsupportedFeature, e.Feature, Version)
}
func (e *FeatureError) Unwrap() error { return ErrUnsupportedFeature }

// Builder errors (CONTRACTS.md §1.5 / §3). Typed errors unwrap to these.
var (
	ErrMissingValueColumn = errors.New("missing attos_value column")
	ErrNoDimensions       = errors.New("no dimension columns")
	ErrBlobTooLarge       = errors.New("blob too large")
	ErrUnplaceable        = errors.New("unplaceable bucket")
	ErrMalformedRow       = errors.New("malformed row")
	ErrDuplicateKey       = errors.New("duplicate key")
	ErrLimit              = errors.New("size limit exceeded")
)

// MalformedRowError reports a row that cannot be parsed. Line is the 1-based
// physical line number (the CSV header is line 1).
type MalformedRowError struct {
	Line   int
	Reason string
}

func (e *MalformedRowError) Error() string {
	return fmt.Sprintf("line %d: %v: %s", e.Line, ErrMalformedRow, e.Reason)
}
func (e *MalformedRowError) Unwrap() error { return ErrMalformedRow }

// DuplicateKeyError reports a key that appears more than once.
type DuplicateKeyError struct {
	FirstLine, Line int
	Key             string // the duplicated key, truncated to 64 bytes (may be empty)
}

func (e *DuplicateKeyError) Error() string {
	if e.Key != "" {
		return fmt.Sprintf("line %d: %v %q (first seen on line %d)", e.Line, ErrDuplicateKey, e.Key, e.FirstLine)
	}
	return fmt.Sprintf("line %d: %v (first seen on line %d)", e.Line, ErrDuplicateKey, e.FirstLine)
}
func (e *DuplicateKeyError) Unwrap() error { return ErrDuplicateKey }

// LimitError reports a key or value longer than the v1 format allows.
type LimitError struct {
	Line     int
	Field    string // "key" or "value"
	Len, Max int
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("line %d: %v: %s length %d > %d", e.Line, ErrLimit, e.Field, e.Len, e.Max)
}
func (e *LimitError) Unwrap() error { return ErrLimit }
