package attos

// Delta sync (docs/DELTA_UPDATES.md in attos-cloud; CONTRACTS.md §4 addendum): a node that is one
// version behind downloads a patch ("ATDL" v1) instead of the whole blob, rebuilds the new blob into a
// temp file from its current file and then goes through exactly the same signature check, CRC
// validation, rename and atomic publish as a full download. The patch format is transport-only; the
// .attos file format is untouched. Any failure falls back to the full blob in the same Sync call.
//
// Patch layout (big-endian): "ATDL" | u32 version=1 | u64 baseSize | u32 baseCRC (the base blob's CRC32
// trailer) | u64 targetSize | [32]byte targetSHA256 | ops | u32 CRC32 of all preceding bytes, where ops
// are 0x00 END, 0x01 COPY uvarint(baseOff) uvarint(len), 0x02 ADD uvarint(len) bytes.

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
)

const (
	deltaMagic   = "ATDL"
	deltaVersion = 1 // copy/add patches
	// deltaVersionTail patches (ATDL v2) rebuild a file from the unchanged main part of the base plus a new
	// overflow tail: op 0x01 TRUNCATE_AND_APPEND, u64 BE baseEnd, u64 BE payloadLen, payload.
	deltaVersionTail   = 2
	deltaOpTruncAppend = 0x01
	deltaHdrLen        = 60

	deltaOpEnd  = 0x00
	deltaOpCopy = 0x01
	deltaOpAdd  = 0x02
)

// Delta request/response headers (opt-in; servers that do not know them ignore the request header).
const (
	deltaAcceptHeader = "X-Attos-Accept-Delta"
	deltaHeader       = "X-Attos-Delta"
	deltaBaseHeader   = "X-Attos-Delta-Base-ETag"
	deltaSHAHeader    = "X-Attos-Delta-Target-SHA256"
)

var (
	errDeltaCorrupt = errors.New("attos: corrupt delta patch")
	errDeltaBase    = errors.New("attos: delta patch does not apply to the current file")
)

// deltaFailure marks a failed delta attempt; the caller falls back to the full blob.
type deltaFailure struct{ err error }

func (d *deltaFailure) Error() string { return "delta sync failed: " + d.err.Error() }
func (d *deltaFailure) Unwrap() error { return d.err }

type deltaHeaderInfo struct {
	BaseSize   uint64
	BaseCRC    uint32
	TargetSize uint64
	TargetSHA  [32]byte
}

// applyDelta reads the patch, copies from base and writes the reconstructed target
// to dst. It verifies the patch CRC, base identity, the output size and the
// target SHA-256; on any error dst may hold partial output and must be
// discarded by the caller.
func applyDelta(dst io.Writer, base io.ReaderAt, baseSize int64, patch io.Reader) (deltaHeaderInfo, error) {
	var h deltaHeaderInfo
	raw := bufio.NewReaderSize(patch, 1<<20)
	br := &deltaCRCReader{r: raw, crc: crc32.NewIEEE()}
	var hdr [deltaHdrLen]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return h, fmt.Errorf("%w: header: %v", errDeltaCorrupt, err)
	}
	if string(hdr[:4]) != deltaMagic || (binary.BigEndian.Uint32(hdr[4:]) != deltaVersion && binary.BigEndian.Uint32(hdr[4:]) != deltaVersionTail) {
		return h, fmt.Errorf("%w: bad magic or version", errDeltaCorrupt)
	}
	tailPatch := binary.BigEndian.Uint32(hdr[4:]) == deltaVersionTail
	h.BaseSize = binary.BigEndian.Uint64(hdr[8:])
	h.BaseCRC = binary.BigEndian.Uint32(hdr[16:])
	h.TargetSize = binary.BigEndian.Uint64(hdr[20:])
	copy(h.TargetSHA[:], hdr[28:])
	if h.BaseSize != uint64(baseSize) {
		return h, fmt.Errorf("%w: base size %d, patch expects %d", errDeltaBase, baseSize, h.BaseSize)
	}
	var tail [4]byte
	if baseSize < 4 {
		return h, fmt.Errorf("%w: base too short", errDeltaBase)
	}
	if _, err := base.ReadAt(tail[:], baseSize-4); err != nil {
		return h, fmt.Errorf("%w: reading base trailer: %v", errDeltaBase, err)
	}
	if binary.BigEndian.Uint32(tail[:]) != h.BaseCRC {
		return h, fmt.Errorf("%w: base CRC differs", errDeltaBase)
	}
	sum := sha256.New()
	out := io.MultiWriter(dst, sum)
	var produced uint64
	buf := make([]byte, 256<<10)
	for {
		op, err := br.ReadByte()
		if err == nil && tailPatch && op == deltaOpTruncAppend {
			if produced != 0 {
				return h, fmt.Errorf("%w: second tail op", errDeltaCorrupt)
			}
			var ph [16]byte
			if _, err := io.ReadFull(br, ph[:]); err != nil {
				return h, fmt.Errorf("%w: tail op: %v", errDeltaCorrupt, err)
			}
			baseEnd, plen := binary.BigEndian.Uint64(ph[:]), binary.BigEndian.Uint64(ph[8:])
			if baseEnd > h.BaseSize-4 || baseEnd < 64 || plen > h.TargetSize || baseEnd != h.TargetSize-plen {
				return h, fmt.Errorf("%w: tail op bounds", errDeltaCorrupt)
			}
			// New file = the base's main part with the overflow flag set + the payload (overflow, footer, CRC).
			var first [64]byte
			if _, err := base.ReadAt(first[:], 0); err != nil {
				return h, fmt.Errorf("%w: reading base header: %v", errDeltaBase, err)
			}
			first[8] |= flagOverflow
			if _, err := out.Write(first[:]); err != nil {
				return h, err
			}
			if m, err := io.CopyBuffer(out, io.NewSectionReader(base, 64, int64(baseEnd)-64), buf); err != nil || uint64(m) != baseEnd-64 {
				return h, fmt.Errorf("attos: delta copy: %d bytes: %v", m, err)
			}
			if m, err := io.CopyBuffer(out, io.LimitReader(br, int64(plen)), buf); err != nil || uint64(m) != plen {
				return h, fmt.Errorf("%w: truncated tail payload", errDeltaCorrupt)
			}
			produced = baseEnd + plen
			continue
		}
		if err != nil {
			return h, fmt.Errorf("%w: truncated: %v", errDeltaCorrupt, err)
		}
		switch op {
		case deltaOpEnd:
			// The CRC covers everything up to and including END.
			want := br.crc.Sum32()
			var t [4]byte
			if _, err := io.ReadFull(raw, t[:]); err != nil {
				return h, fmt.Errorf("%w: trailer: %v", errDeltaCorrupt, err)
			}
			if binary.BigEndian.Uint32(t[:]) != want {
				return h, fmt.Errorf("%w: patch CRC mismatch", errDeltaCorrupt)
			}
			if _, err := raw.ReadByte(); err != io.EOF {
				return h, fmt.Errorf("%w: trailing bytes", errDeltaCorrupt)
			}
			if produced != h.TargetSize {
				return h, fmt.Errorf("%w: produced %d of %d bytes", errDeltaCorrupt, produced, h.TargetSize)
			}
			var got [32]byte
			copy(got[:], sum.Sum(nil))
			if got != h.TargetSHA {
				return h, fmt.Errorf("%w: target SHA-256 mismatch", errDeltaCorrupt)
			}
			return h, nil
		case deltaOpCopy, deltaOpAdd:
			var off uint64
			if op == deltaOpCopy {
				if off, err = binary.ReadUvarint(br); err != nil {
					return h, fmt.Errorf("%w: copy offset: %v", errDeltaCorrupt, err)
				}
			}
			n, err := binary.ReadUvarint(br)
			if err != nil {
				return h, fmt.Errorf("%w: length: %v", errDeltaCorrupt, err)
			}
			if n > h.TargetSize-produced {
				return h, fmt.Errorf("%w: op overruns target", errDeltaCorrupt)
			}
			if op == deltaOpCopy {
				if off > h.BaseSize || n > h.BaseSize-off {
					return h, fmt.Errorf("%w: copy outside base", errDeltaCorrupt)
				}
				m, err := io.CopyBuffer(out, io.NewSectionReader(base, int64(off), int64(n)), buf)
				if err != nil || uint64(m) != n {
					return h, fmt.Errorf("attos: delta copy: %d of %d bytes: %v", m, n, err)
				}
			} else {
				m, err := io.CopyBuffer(out, io.LimitReader(br, int64(n)), buf)
				if err != nil {
					return h, fmt.Errorf("attos: delta add: %w", err)
				}
				if uint64(m) != n {
					return h, fmt.Errorf("%w: truncated add", errDeltaCorrupt)
				}
			}
			produced += n
		default:
			return h, fmt.Errorf("%w: unknown op %#x", errDeltaCorrupt, op)
		}
	}
}

// deltaCRCReader hashes exactly the bytes handed to the caller (not bufio's
// read-ahead), so the patch CRC covers everything up to the END op.
type deltaCRCReader struct {
	r   *bufio.Reader
	crc hash.Hash32
}

func (c *deltaCRCReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.crc.Write(p[:n])
	return n, err
}

func (c *deltaCRCReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.crc.Write([]byte{b})
	}
	return b, err
}
