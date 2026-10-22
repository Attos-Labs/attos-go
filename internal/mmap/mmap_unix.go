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

// Mlock locks the mapped memory into RAM, preventing it from being swapped out.
func Mlock(data []byte) error {
	if data == nil {
		return nil
	}
	return syscall.Mlock(data)
}

// Munlock unlocks the mapped memory.
func Munlock(data []byte) error {
	if data == nil {
		return nil
	}
	return syscall.Munlock(data)
}

// AdviseRandom tells the kernel the mapping is accessed randomly: no readahead, so a lookup only
// brings in the pages it touches.
func AdviseRandom(data []byte) error {
	if data == nil {
		return nil
	}
	return syscall.Madvise(data, syscall.MADV_RANDOM)
}

// Release drops the mapping's resident pages from the process (the page cache keeps them). Used after
// a full-file pass such as CRC validation so the whole file does not stay counted in the process RSS.
func Release(data []byte) error {
	if data == nil {
		return nil
	}
	return syscall.Madvise(data, syscall.MADV_DONTNEED)
}
