package attos

// Patch sync (docs/PATCH_UPDATES.md): a node that holds the main blob of a chain of overflow variants, or any
// variant of it, downloads one small patch ("ATDL" v2, a tail patch) instead of the whole file, rebuilds the new
// file into a temp file from its current file and then goes through exactly the same signature check, CRC
// validation, rename and atomic publish as a full download. The patch format is transport-only; the .attos
// file format is untouched. Any failure falls back to the full file in the same Sync call.
//
// Patch layout (big-endian): "ATDL" | u32 version=2 | u64 baseSize | u32 baseCRC | u64 targetSize |
// [32]byte targetSHA256 | op 0x01 TRUNCATE_AND_APPEND | u64 baseEnd | u64 payloadLen | payload | op 0x00 END |
// u32 CRC32 of all preceding bytes. The new file is the first baseEnd bytes of the base (header flag bit set)
// followed by the payload. baseSize and baseCRC describe the base the server built the patch from and are not
// checked: the patch applies to any file that starts with the same main blob, and the target SHA-256 proves
// that it did.

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
	deltaMagic         = "ATDL"
	deltaVersion       = 2 // tail patches
	deltaOpTruncAppend = 0x01
	deltaHdrLen        = 60
	deltaOpEnd         = 0x00
)

// Patch response headers and the capability request header.
const (
	noRedirectHeader   = "X-Attos-No-Redirect"      // request: stream the bytes yourself (fallback after a failed signed download)
	contentLengthHdr   = "X-Attos-Content-Length"   // 302: size of the object behind the URL
	contentEncodingHdr = "X-Attos-Content-Encoding" // 302: content coding of the object behind the URL
	capabilitiesHeader = "X-Attos-Capabilities"     // comma-separated: "patch" (read overflow files, apply tail patches), "redirect"
	deltaHeader        = "X-Attos-Delta"
	deltaSHAHeader     = "X-Attos-Delta-Target-SHA256"
)

var errDeltaCorrupt = errors.New("attos: corrupt delta patch")

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

// applyDelta reads the patch, copies the main blob from base and writes the reconstructed target to dst. It
// verifies the patch CRC, the output size and the target SHA-256; on any error dst may hold partial output and
// must be discarded by the caller.
func applyDelta(dst io.Writer, base io.ReaderAt, baseSize int64, patch io.Reader) (deltaHeaderInfo, error) {
	var h deltaHeaderInfo
	raw := bufio.NewReaderSize(patch, 1<<20)
	br := &deltaCRCReader{r: raw, crc: crc32.NewIEEE()}
	var hdr [deltaHdrLen]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return h, fmt.Errorf("%w: header: %v", errDeltaCorrupt, err)
	}
	if string(hdr[:4]) != deltaMagic || binary.BigEndian.Uint32(hdr[4:]) != deltaVersion {
		return h, fmt.Errorf("%w: bad magic or version", errDeltaCorrupt)
	}
	h.BaseSize = binary.BigEndian.Uint64(hdr[8:])
	h.BaseCRC = binary.BigEndian.Uint32(hdr[16:])
	h.TargetSize = binary.BigEndian.Uint64(hdr[20:])
	copy(h.TargetSHA[:], hdr[28:])
	sum := sha256.New()
	out := io.MultiWriter(dst, sum)
	var produced uint64
	buf := make([]byte, 256<<10)
	for {
		op, err := br.ReadByte()
		if err != nil {
			return h, fmt.Errorf("%w: truncated: %v", errDeltaCorrupt, err)
		}
		switch op {
		case deltaOpTruncAppend:
			if produced != 0 {
				return h, fmt.Errorf("%w: second tail op", errDeltaCorrupt)
			}
			var ph [16]byte
			if _, err := io.ReadFull(br, ph[:]); err != nil {
				return h, fmt.Errorf("%w: tail op: %v", errDeltaCorrupt, err)
			}
			baseEnd, plen := binary.BigEndian.Uint64(ph[:]), binary.BigEndian.Uint64(ph[8:])
			if baseSize < 4 || baseEnd > uint64(baseSize)-4 || baseEnd < 64 || plen > h.TargetSize || baseEnd != h.TargetSize-plen {
				return h, fmt.Errorf("%w: tail op bounds", errDeltaCorrupt)
			}
			// New file = the base's main part with the overflow flag set + the payload (overflow, footer, CRC).
			var first [64]byte
			if _, err := base.ReadAt(first[:], 0); err != nil {
				return h, fmt.Errorf("attos: reading base header: %v", err)
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
				return h, fmt.Errorf("%w: target SHA-256 mismatch (the base is not a file of the same chain)", errDeltaCorrupt)
			}
			return h, nil
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
