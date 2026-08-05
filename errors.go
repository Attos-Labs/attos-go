package attos

import "errors"

var (
	ErrKeyNotFound      = errors.New("key not found")
	ErrCorruptedBinary  = errors.New("corrupted binary")
	ErrInvalidMagic     = errors.New("invalid magic bytes")
	ErrInvalidVersion   = errors.New("invalid version")
	ErrChecksumMismatch = errors.New("checksum mismatch")
	ErrMapClosed        = errors.New("map closed")
	ErrMlockFailed      = errors.New("mlock failed")
)
