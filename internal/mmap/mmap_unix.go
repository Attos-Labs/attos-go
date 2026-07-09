//go:build unix
// +build unix

package mmap

import (
	"fmt"
	"os"
	"syscall"
)

// MapFile maps a file read-only using syscall.Mmap.
func MapFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	if size == 0 {
		return nil, fmt.Errorf("empty file")
	}

	data, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Unmap unmaps the memory-mapped byte slice.
func Unmap(data []byte) error {
	if data == nil {
		return nil
	}
	return syscall.Munmap(data)
}
