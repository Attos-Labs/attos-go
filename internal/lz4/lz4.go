// Package lz4 is a small, dependency-free implementation of the LZ4 block format
// (https://github.com/lz4/lz4/blob/dev/doc/lz4_Block_format.md): a greedy compressor used when
// the SDK builds compressed .attos files, and a bounds-checked decompressor used by readers.
package lz4

import (
	"encoding/binary"
	"errors"
)

var ErrCorrupt = errors.New("lz4: corrupt block")

// Slack is the extra capacity Decompress wants behind dst for its fast paths.
const Slack = 40

const (
	minMatch  = 4
	lastLits  = 5  // the last 5 bytes are always literals
	mfLimit   = 12 // a match must start at least 12 bytes before the end
	hashLog   = 12
	maxOffset = 65535
)

// CompressBound is the worst-case compressed size of n input bytes.
func CompressBound(n int) int { return n + n/255 + 16 }

func hash4(v uint32) uint32 { return (v * 2654435761) >> (32 - hashLog) }

// Compress appends the LZ4 block encoding of src to dst. It returns dst unchanged-extended; the
// block may be larger than src for incompressible input, so callers should compare sizes.
func Compress(dst, src []byte) []byte {
	n := len(src)
	var table [1 << hashLog]int32 // position+1 of the last occurrence of a hash
	anchor, si := 0, 0
	if n >= mfLimit+1 {
		limit := n - mfLimit
		for si < limit {
			h := hash4(binary.LittleEndian.Uint32(src[si:]))
			ref := int(table[h]) - 1
			table[h] = int32(si) + 1
			if ref < 0 || si-ref > maxOffset || binary.LittleEndian.Uint32(src[ref:]) != binary.LittleEndian.Uint32(src[si:]) {
				si++
				continue
			}
			// extend the match forward (never into the last 5 bytes)
			ml := minMatch
			for si+ml < n-lastLits && src[ref+ml] == src[si+ml] {
				ml++
			}
			dst = emitSequence(dst, src[anchor:si], si-ref, ml)
			si += ml
			anchor = si
		}
	}
	// final literals
	lit := src[anchor:]
	tok := byte(0)
	if len(lit) >= 15 {
		tok = 15 << 4
	} else {
		tok = byte(len(lit)) << 4
	}
	dst = append(dst, tok)
	if len(lit) >= 15 {
		dst = appendLen(dst, len(lit)-15)
	}
	return append(dst, lit...)
}

func appendLen(dst []byte, l int) []byte {
	for l >= 255 {
		dst = append(dst, 255)
		l -= 255
	}
	return append(dst, byte(l))
}

func emitSequence(dst, lit []byte, offset, matchLen int) []byte {
	var tok byte
	if len(lit) >= 15 {
		tok = 15 << 4
	} else {
		tok = byte(len(lit)) << 4
	}
	ml := matchLen - minMatch
	if ml >= 15 {
		tok |= 15
	} else {
		tok |= byte(ml)
	}
	dst = append(dst, tok)
	if len(lit) >= 15 {
		dst = appendLen(dst, len(lit)-15)
	}
	dst = append(dst, lit...)
	dst = append(dst, byte(offset), byte(offset>>8))
	if ml >= 15 {
		dst = appendLen(dst, ml-15)
	}
	return dst
}

// Decompress decodes src into dst, which must have the exact decoded length. Every access is bounds checked.
//
// If cap(dst) >= len(dst)+Slack the decoder uses faster 8-byte copies that may write up to Slack
// bytes past the end of dst (within its capacity); those bytes are scratch and never read back.
func Decompress(dst, src []byte) error {
	full := dst[:cap(dst)]
	si, di := 0, 0
	for {
		if si >= len(src) {
			return ErrCorrupt
		}
		tok := src[si]
		si++
		litLen := int(tok >> 4)
		if litLen == 15 {
			for {
				if si >= len(src) {
					return ErrCorrupt
				}
				b := src[si]
				si++
				litLen += int(b)
				if b != 255 {
					break
				}
				if litLen > len(dst) {
					return ErrCorrupt
				}
			}
		}
		if litLen > len(src)-si || litLen > len(dst)-di {
			return ErrCorrupt
		}
		if litLen <= 16 && si+16 <= len(src) && di+16 <= len(full) {
			binary.LittleEndian.PutUint64(full[di:], binary.LittleEndian.Uint64(src[si:]))
			binary.LittleEndian.PutUint64(full[di+8:], binary.LittleEndian.Uint64(src[si+8:]))
		} else {
			copy(dst[di:], src[si:si+litLen])
		}
		si += litLen
		di += litLen
		if si == len(src) {
			if di != len(dst) {
				return ErrCorrupt
			}
			return nil
		}
		if si+2 > len(src) {
			return ErrCorrupt
		}
		offset := int(src[si]) | int(src[si+1])<<8
		si += 2
		if offset == 0 || offset > di {
			return ErrCorrupt
		}
		ml := int(tok & 15)
		if ml == 15 {
			for {
				if si >= len(src) {
					return ErrCorrupt
				}
				b := src[si]
				si++
				ml += int(b)
				if b != 255 {
					break
				}
				if ml > len(dst) {
					return ErrCorrupt
				}
			}
		}
		ml += minMatch
		if ml > len(dst)-di {
			return ErrCorrupt
		}
		from := di - offset
		if offset >= 8 && ml <= 32 && di+ml+Slack <= len(full) {
			// 8-byte chunks; the final chunk may overshoot by up to 7 bytes (into the slack)
			for k := 0; k < ml; k += 8 {
				binary.LittleEndian.PutUint64(full[di+k:], binary.LittleEndian.Uint64(full[from+k:]))
			}
		} else if offset >= ml {
			copy(dst[di:di+ml], dst[from:from+ml])
		} else {
			// overlapping match: copy the pattern forward in doubling chunks
			end := di + ml
			for di < end {
				n := copy(dst[di:end], dst[from:di])
				di += n
			}
			continue
		}
		di += ml
	}
}
