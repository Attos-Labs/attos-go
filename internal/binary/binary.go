package binary

import (
	"unsafe"
)

// BytesToUint64Slice safely aliases a byte slice to a uint64 slice.
// This relies on the byte slice being correctly aligned.
func BytesToUint64Slice(b []byte) []uint64 {
	n := len(b) / 8
	if n == 0 {
		return nil
	}
	return unsafe.Slice((*uint64)(unsafe.Pointer(unsafe.SliceData(b))), n)
}

// BytesToUint32Slice safely aliases a byte slice to a uint32 slice.
func BytesToUint32Slice(b []byte) []uint32 {
	n := len(b) / 4
	if n == 0 {
		return nil
	}
	return unsafe.Slice((*uint32)(unsafe.Pointer(unsafe.SliceData(b))), n)
}
