package attos

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"math/bits"
	"os"

	"github.com/Attos-Labs/attos-go/internal/lz4"
	"github.com/Attos-Labs/attos-go/internal/pthash"
	"github.com/cespare/xxhash/v2"
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
	// Fail fast: the uncompressed blob size is known before any hashing.
	if opts.Compression == CompressNone {
		if b.dataBytes > maxBlob40 {
			return fmt.Errorf("blob of %d bytes exceeds the 1 TiB limit of version 2", b.dataBytes)
		}
		if opts.OffsetWidth == 4 && b.dataBytes > math.MaxUint32 {
			return fmt.Errorf("blob of %d bytes needs OffsetWidth 5 (4 GiB limit of 32-bit offsets)", b.dataBytes)
		}
	}
	if b.checked < n {
		if d := b.firstDuplicate(b.checked); d != nil {
			return d
		}
		b.checked = n
	}
	xs := make([]uint64, n)
	for i := range xs {
		k := b.rowKey(i)
		if len(k) > math.MaxUint16 {
			return fmt.Errorf("key %d too long (%d bytes, max 65535)", i, len(k))
		}
		xs[i] = xxhash.Sum64(k)
	}
	t, err := pthash.Build(xs)
	if err != nil {
		return err
	}
	slotEntry := make([]int32, n)
	for i, x := range xs {
		slotEntry[t.Slot(x)] = int32(i)
	}
	var offsets []uint64
	var blob []byte // only used when compressing
	if opts.Compression == CompressLZ4 {
		var err error
		offsets, blob, err = b.compressBlocks(slotEntry, shift)
		if err != nil {
			return err
		}
	} else {
		offsets = make([]uint64, n+1)
		var total uint64
		for s := 0; s < n; s++ {
			ei := int(slotEntry[s])
			ek, ev := b.rowKey(ei), b.rowValue(ei)
			offsets[s] = total
			total += 2 + uint64(len(ek)) + uint64(len(ev))
		}
		offsets[n] = total
	}
	width := opts.OffsetWidth
	total := offsets[len(offsets)-1]
	switch {
	case total > maxBlob40:
		return fmt.Errorf("blob of %d bytes exceeds the 1 TiB limit of version 2", total)
	case width == 0 && total <= math.MaxUint32:
		width = 4
	case width == 0:
		width = 5
	case width == 4 && total > math.MaxUint32:
		return fmt.Errorf("blob of %d bytes needs OffsetWidth 5 (4 GiB limit of 32-bit offsets)", total)
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
	ob := make([]byte, offsetsSize(uint64(len(offsets)), width))
	for i, o := range offsets {
		if width == 5 {
			var tmp [8]byte
			binary.LittleEndian.PutUint64(tmp[:], o)
			copy(ob[i*5:], tmp[:5])
		} else {
			binary.LittleEndian.PutUint32(ob[i*4:], uint32(o))
		}
	}
	w.Write(ob)
	if opts.Compression == CompressLZ4 {
		w.Write(blob)
	} else {
		var lenBuf [2]byte
		for s := 0; s < n; s++ {
			ei := int(slotEntry[s])
			ek, ev := b.rowKey(ei), b.rowValue(ei)
			be.PutUint16(lenBuf[:], uint16(len(ek)))
			w.Write(lenBuf[:])
			w.Write(ek)
			w.Write(ev)
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
// A block is stored raw when LZ4 does not make it smaller.
func (b *Builder) compressBlocks(slotEntry []int32, shift uint8) ([]uint64, []byte, error) {
	n := len(slotEntry)
	per := 1 << shift
	nBlocks := (n + per - 1) / per
	offsets := make([]uint64, nBlocks+1)
	var blob, raw, comp []byte
	var vb [binary.MaxVarintLen64]byte
	for blk := 0; blk < nBlocks; blk++ {
		offsets[blk] = uint64(len(blob))
		raw = raw[:0]
		for s := blk * per; s < (blk+1)*per && s < n; s++ {
			ei := int(slotEntry[s])
			ek, ev := b.rowKey(ei), b.rowValue(ei)
			rl := 2 + len(ek) + len(ev)
			raw = append(raw, vb[:binary.PutUvarint(vb[:], uint64(rl))]...)
			raw = append(raw, byte(len(ek)>>8), byte(len(ek)))
			raw = append(raw, ek...)
			raw = append(raw, ev...)
		}
		comp = lz4.Compress(comp[:0], raw)
		method, payload := byte(blockLZ4), comp
		if len(comp) >= len(raw) {
			method, payload = blockStored, raw
		}
		blob = append(blob, method)
		blob = append(blob, vb[:binary.PutUvarint(vb[:], uint64(len(raw)))]...)
		blob = append(blob, payload...)
		if uint64(len(blob)) > maxBlob40 {
			return nil, nil, fmt.Errorf("blob of %d bytes exceeds the 1 TiB limit of version 2", len(blob))
		}
	}
	offsets[nBlocks] = uint64(len(blob))
	return offsets, blob, nil
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
