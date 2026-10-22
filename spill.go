package attos

import (
	"bufio"
	"fmt"
	"os"

	"github.com/Attos-Labs/attos-go/internal/mmap"
)

// spillStore keeps the key and value bytes of added rows in a temporary file instead of memory, so a Builder
// holds only about 26 bytes of index per row regardless of value size. Appends are buffered; reads go through
// a read-only mapping that is refreshed when the file has grown.
type spillStore struct {
	path   string
	f      *os.File
	w      *bufio.Writer
	size   uint64 // bytes appended so far (including buffered ones)
	mapped []byte
}

func newSpillStore(dir string) (*spillStore, error) {
	f, err := os.CreateTemp(dir, "attos-spill-*")
	if err != nil {
		return nil, err
	}
	return &spillStore{path: f.Name(), f: f, w: bufio.NewWriterSize(f, 1<<20)}, nil
}

func (s *spillStore) append(key, value []byte) uint64 {
	off := s.size
	s.w.Write(key) // errors surface at the next flush
	s.w.Write(value)
	s.size += uint64(len(key) + len(value))
	return off
}

// view returns n bytes at off, mapping newly written data when needed.
func (s *spillStore) view(off uint64, n int) []byte {
	if off+uint64(n) > uint64(len(s.mapped)) {
		if err := s.remap(); err != nil {
			panic(fmt.Sprintf("attos: spill file: %v", err))
		}
	}
	return s.mapped[off : off+uint64(n)]
}

func (s *spillStore) remap() error {
	if err := s.w.Flush(); err != nil {
		return err
	}
	if s.mapped != nil {
		mmap.Unmap(s.mapped)
		s.mapped = nil
	}
	if s.size == 0 {
		return nil
	}
	data, err := mmap.MapFile(s.path)
	if err != nil {
		return err
	}
	s.mapped = data
	return nil
}

// truncate drops everything after size (used when a failed parse rolls rows back).
func (s *spillStore) truncate(size uint64) error {
	if err := s.w.Flush(); err != nil {
		return err
	}
	if s.mapped != nil {
		mmap.Unmap(s.mapped)
		s.mapped = nil
	}
	if err := s.f.Truncate(int64(size)); err != nil {
		return err
	}
	if _, err := s.f.Seek(int64(size), 0); err != nil {
		return err
	}
	s.w.Reset(s.f)
	s.size = size
	return nil
}

func (s *spillStore) close() {
	if s.mapped != nil {
		mmap.Unmap(s.mapped)
	}
	s.f.Close()
	os.Remove(s.path)
}

// NewSpillBuilder returns a Builder that stores row data in a temporary file under dir ("" means os.TempDir)
// and keeps only a small index in memory. Output is byte-for-byte identical to NewBuilder. Call Close when
// done to remove the temporary file.
func NewSpillBuilder(dir string) (*Builder, error) {
	s, err := newSpillStore(dir)
	if err != nil {
		return nil, err
	}
	return &Builder{spill: s}, nil
}

// Close releases the temporary file of a spilling Builder. It is a no-op for NewBuilder builders.
func (b *Builder) Close() error {
	if b.spill != nil {
		b.spill.close()
		b.spill = nil
	}
	return nil
}
