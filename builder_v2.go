package attos

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/Attos-Labs/attos-go/internal/lz4"
	"github.com/Attos-Labs/attos-go/internal/pthash"
)

const headerSizeV2 = 64

// Compression selects how records are stored in a version 2 file.
type Compression int

const (
	// CompressNone stores records uncompressed: fastest lookups, zero allocations per Get.
	CompressNone Compression = iota
	// CompressLZ4 stores records in small LZ4-compressed blocks. Smaller files, but every lookup
	// decompresses one block (roughly 0.4 microseconds per record in the block) and allocates. See FORMAT_V2.md.
	CompressLZ4
)

// CompileOptions tunes Compile. The zero value means no compression.
type CompileOptions struct {
	Compression Compression
	// BlockSize is the number of records per compressed block: a power of two from 1 to 256.
	// Smaller blocks decompress faster but compress less. Default 8.
	BlockSize int
	// OffsetWidth is the byte width of the offsets section: 0 chooses automatically (4 when the blob is at
	// most 2^32-1 bytes, else 5), 4 forces 32-bit offsets, 5 forces 40-bit offsets (blobs up to 1 TiB).
	OffsetWidth int
	// FingerprintBytes selects lean keys: 0 stores full keys (default); 4 or 8 stores only a fingerprint
	// of that many bytes per record instead of the key (records are fingerprint|value). Lookups verify the
	// fingerprint, so a key that was never added is accepted with probability 2^-32 (4) or 2^-64 (8).
	// Scan then yields the fingerprint in place of the key. See FORMAT_V2.md.
	FingerprintBytes int
}

const maxBlob40 = 1<<40 - 1

// Compile writes a version 2 (.attos) file without compression. See CompileWithOptions.
func (b *Builder) Compile(outPath string) error {
	return b.CompileWithOptions(outPath, CompileOptions{})
}

// CompileWithOptions writes a version 2 (.attos) file: a PTHash minimal perfect hash, an offset table
// and the key-prefixed records stored in slot order, optionally in LZ4 blocks. See FORMAT_V2.md.
func (b *Builder) CompileWithOptions(outPath string, opts CompileOptions) error {
	shift := uint8(0)
	if opts.Compression == CompressLZ4 {
		bs := opts.BlockSize
		if bs == 0 {
			bs = 8
		}
		if bs < 1 || bs > 256 || bs&(bs-1) != 0 {
			return errors.New("BlockSize must be a power of two between 1 and 256")
		}
		shift = uint8(bits.TrailingZeros(uint(bs)))
	} else if opts.Compression != CompressNone {
		return errors.New("unknown compression")
	}
	n := b.Len()
	if n == 0 {
		return errors.New("no entries to compile")
	}
	if opts.OffsetWidth != 0 && opts.OffsetWidth != 4 && opts.OffsetWidth != 5 {
		return errors.New("OffsetWidth must be 0 (auto), 4 or 5")
	}
	fpW := opts.FingerprintBytes
	if fpW != 0 && fpW != 4 && fpW != 8 {
		return errors.New("FingerprintBytes must be 0 (full keys), 4 or 8")
	}
	blobBytes := b.dataBytes // sum over rows of 2 + key + value
	if fpW != 0 {
		blobBytes = 0
		for i := range b.vlens {
			blobBytes += uint64(fpW) + uint64(b.vlens[i])
		}
	}
	// Fail fast: the uncompressed blob size is known before any hashing.
	if opts.Compression == CompressNone {
		if blobBytes > maxBlob40 {
			return fmt.Errorf("blob of %d bytes exceeds the 1 TiB limit of version 2", blobBytes)
		}
		if opts.OffsetWidth == 4 && blobBytes > math.MaxUint32 {
			return fmt.Errorf("blob of %d bytes needs OffsetWidth 5 (4 GiB limit of 32-bit offsets)", blobBytes)
		}
	}
	if b.checked < n {
		if d := b.firstDuplicate(b.checked); d != nil {
			return d
		}
		b.checked = n
	}
	b.ensureHashes() // XXH64 of every key; kept in the Builder, so no second copy is made
	xs := b.hashes[:n]
	if b.spill != nil {
		// Keep garbage low while the index dominates the heap; restore the caller's setting afterwards.
		defer debug.SetGCPercent(debug.SetGCPercent(20))
	}
	t, err := pthash.Build(xs)
	if err != nil {
		return fmt.Errorf("%w (two different keys with the same 64-bit hash?)", err)
	}
	slotEntry := make([]int32, n)
	for i, x := range xs {
		slotEntry[t.Slot(x)] = int32(i)
	}

	// Compressed blocks go to a temporary file (spilling builders) or memory; their offsets are small.
	var blockOffsets []uint64
	var blobSrc io.Reader
	var blobLen uint64
	if opts.Compression == CompressLZ4 {
		src, offs, cleanup, err := b.compressBlocks(slotEntry, shift, fpW)
		if err != nil {
			return err
		}
		defer cleanup()
		blockOffsets, blobSrc, blobLen = offs, src, offs[len(offs)-1]
	} else {
		blobLen = blobBytes
	}
	width := opts.OffsetWidth
	switch {
	case blobLen > maxBlob40:
		return fmt.Errorf("blob of %d bytes exceeds the 1 TiB limit of version 2", blobLen)
	case width == 0 && blobLen <= math.MaxUint32:
		width = 4
	case width == 0:
		width = 5
	case width == 4 && blobLen > math.MaxUint32:
		return fmt.Errorf("blob of %d bytes needs OffsetWidth 5 (4 GiB limit of 32-bit offsets)", blobLen)
	}

	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	bw := bufio.NewWriterSize(f, 4<<20)
	crc := crc32.NewIEEE()
	w := io.MultiWriter(bw, crc)

	hdr := make([]byte, headerSizeV2)
	copy(hdr, magicATTO)
	be := binary.BigEndian
	be.PutUint32(hdr[4:], 2)
	if opts.Compression == CompressLZ4 {
		hdr[8] = 1
		hdr[11] = shift
	}
	if fpW != 0 {
		hdr[8] |= 2
		hdr[53] = byte(fpW)
	}
	hdr[9], hdr[10] = t.FW, t.BW
	if width == 5 {
		hdr[52] = 5
	}
	be.PutUint64(hdr[12:], t.N)
	be.PutUint64(hdr[20:], t.M)
	be.PutUint64(hdr[28:], t.NB)
	be.PutUint64(hdr[36:], t.P2)
	be.PutUint64(hdr[44:], t.P1Thr)
	w.Write(hdr)
	w.Write(t.Front)
	w.Write(t.Back)
	w.Write(t.Free)
	if opts.Compression == CompressLZ4 {
		if err := writeOffsets(w, uint64(len(blockOffsets)), width, func(i uint64) uint64 { return blockOffsets[i] }); err != nil {
			return err
		}
		if _, err := io.Copy(w, blobSrc); err != nil {
			return err
		}
	} else {
		// Offsets come from the stored lengths alone: no record bytes are read in this pass.
		var run uint64
		next := func(i uint64) uint64 {
			o := run
			if i < uint64(n) {
				ei := slotEntry[i]
				if fpW != 0 {
					run += uint64(fpW) + uint64(b.vlens[ei])
				} else {
					run += 2 + uint64(b.klens[ei]) + uint64(b.vlens[ei])
				}
			}
			return o
		}
		if err := writeOffsets(w, uint64(n)+1, width, next); err != nil {
			return err
		}
		var lenBuf [2]byte
		for s := 0; s < n; s++ {
			ei := int(slotEntry[s])
			if fpW != 0 {
				fp := keyFingerprint(b.rowKey(ei))
				w.Write(fp[:fpW])
			} else {
				be.PutUint16(lenBuf[:], b.klens[ei])
				w.Write(lenBuf[:])
				w.Write(b.rowKey(ei))
			}
			w.Write(b.rowValue(ei))
		}
	}
	var tr [4]byte
	be.PutUint32(tr[:], crc.Sum32())
	bw.Write(tr[:])
	if err := bw.Flush(); err != nil {
		return err
	}
	return f.Sync()
}

// compressBlocks packs the records of consecutive slots into blocks of 1<<shift records:
// method byte | uvarint(raw length) | payload, where raw = for each record uvarint(len) | record.
// A block is stored raw when LZ4 does not make it smaller. The blob is returned as a reader (a temporary
// file for spilling builders, memory otherwise) together with the block offsets; cleanup releases it.
func (b *Builder) compressBlocks(slotEntry []int32, shift uint8, fpW int) (io.Reader, []uint64, func(), error) {
	n := len(slotEntry)
	per := 1 << shift
	nBlocks := (n + per - 1) / per
	offsets := make([]uint64, nBlocks+1)

	var out io.Writer
	var mem *bytes.Buffer
	var tmp *os.File
	cleanup := func() {}
	if b.spill != nil {
		var err error
		tmp, err = os.CreateTemp(filepath.Dir(b.spill.path), "attos-blob-*")
		if err != nil {
			return nil, nil, nil, err
		}
		cleanup = func() { tmp.Close(); os.Remove(tmp.Name()) }
		out = bufio.NewWriterSize(tmp, 4<<20)
	} else {
		mem = &bytes.Buffer{}
		out = mem
	}
	var written uint64
	var raw, comp []byte
	var vb [binary.MaxVarintLen64]byte
	for blk := 0; blk < nBlocks; blk++ {
		offsets[blk] = written
		raw = raw[:0]
		for s := blk * per; s < (blk+1)*per && s < n; s++ {
			ei := int(slotEntry[s])
			ek, ev := b.rowKey(ei), b.rowValue(ei)
			if fpW != 0 {
				fp := keyFingerprint(ek)
				raw = append(raw, vb[:binary.PutUvarint(vb[:], uint64(fpW+len(ev)))]...)
				raw = append(raw, fp[:fpW]...)
			} else {
				rl := 2 + len(ek) + len(ev)
				raw = append(raw, vb[:binary.PutUvarint(vb[:], uint64(rl))]...)
				raw = append(raw, byte(len(ek)>>8), byte(len(ek)))
				raw = append(raw, ek...)
			}
			raw = append(raw, ev...)
		}
		comp = lz4.Compress(comp[:0], raw)
		method, payload := byte(blockLZ4), comp
		if len(comp) >= len(raw) {
			method, payload = blockStored, raw
		}
		hl := binary.PutUvarint(vb[:], uint64(len(raw)))
		out.Write([]byte{method})
		out.Write(vb[:hl])
		out.Write(payload)
		written += uint64(1 + hl + len(payload))
		if written > maxBlob40 {
			cleanup()
			return nil, nil, nil, fmt.Errorf("blob of %d bytes exceeds the 1 TiB limit of version 2", written)
		}
	}
	offsets[nBlocks] = written
	if tmp == nil {
		return mem, offsets, cleanup, nil
	}
	if err := out.(*bufio.Writer).Flush(); err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	return tmp, offsets, cleanup, nil
}

// writeOffsets writes count offsets (next(i) for i in [0, count)) at the given width, padded as offsetsSize says.
func writeOffsets(w io.Writer, count uint64, width int, next func(i uint64) uint64) error {
	buf := make([]byte, 0, 1<<16)
	var tmp [8]byte
	for i := uint64(0); i < count; i++ {
		o := next(i)
		if width == 5 {
			binary.LittleEndian.PutUint64(tmp[:], o)
			buf = append(buf, tmp[:5]...)
		} else {
			binary.LittleEndian.PutUint32(tmp[:], uint32(o))
			buf = append(buf, tmp[:4]...)
		}
		if len(buf) >= cap(buf)-8 {
			if _, err := w.Write(buf); err != nil {
				return err
			}
			buf = buf[:0]
		}
	}
	pad := int(offsetsSize(count, width) - count*uint64(width))
	buf = append(buf, make([]byte, pad)...)
	_, err := w.Write(buf)
	return err
}

// offsetsSize is the byte size of the offsets section for count offsets of the given width. Width 5 adds 8
// bytes of slack so a reader can load an unaligned u64 at any element.
func offsetsSize(count uint64, width int) uint64 {
	sz := (count*uint64(width) + 7) / 8 * 8
	if width == 5 {
		sz += 8
	}
	return sz
}

// keyFingerprint is the lean-keys fingerprint of key: FNV-1a 64 over the key bytes, finished with the
// PTHash mix() so all bits are well distributed; the stored fingerprint is the first 4 or 8 bytes of the
// big-endian result. It is independent of the XXH64 hash that PTHash uses.
func keyFingerprint(key []byte) [8]byte {
	h := uint64(0xcbf29ce484222325)
	for _, c := range key {
		h = (h ^ uint64(c)) * 0x100000001b3
	}
	h ^= h >> 32
	h *= 0xD6E8FEB86659FD93
	h ^= h >> 32
	h *= 0xD6E8FEB86659FD93
	h ^= h >> 32
	var out [8]byte
	binary.BigEndian.PutUint64(out[:], h)
	return out
}
