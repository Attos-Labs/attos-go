// Package pthash implements the minimal perfect hash used by the .attos v2 format
// (PTHash, Pibiri & Trani 2021, "dual compact" pilot encoding).
//
// The same algorithm, constants and bit layout are implemented by the Java and Rust SDKs
// and by the cloud compiler (attos-cloud/engine/pthash). Any change here is a format change.
package pthash

import (
	"encoding/binary"
	"errors"
	"math"
	"math/bits"
)

const (
	// Default build parameters. They are stored in the file header, not recomputed by readers.
	DefaultC     = 6.0
	DefaultAlpha = 0.99

	hpOffset = 0x1234567
	mixMul   = 0xD6E8FEB86659FD93
)

var (
	ErrDuplicateHash = errors.New("pthash: two keys have the same 64-bit hash (duplicate key or hash collision)")
	ErrNoKeys        = errors.New("pthash: no keys")
)

// Mix is a 64-bit finalizer (xorshift-multiply, 3 rounds).
func Mix(x uint64) uint64 {
	x ^= x >> 32
	x *= mixMul
	x ^= x >> 32
	x *= mixMul
	x ^= x >> 32
	return x
}

// Reduce maps a uniform 64-bit h to [0,n): floor(h*n / 2^64).
func Reduce(h, n uint64) uint64 {
	hi, _ := bits.Mul64(h, n)
	return hi
}

// HP hashes a pilot value.
func HP(p uint64) uint64 { return Mix(p + hpOffset) }

// Table is a built or mapped PTHash function. Front/Back are bit-packed little-endian pilot
// arrays followed by 8 bytes of padding so a 64-bit unaligned read at any element is in bounds.
// Free is the little-endian uint32 remap table for positions >= N.
type Table struct {
	N, M, NB, P2, P1Thr uint64
	FW, BW              uint8
	Front, Back, Free   []byte
}

func getBits(data []byte, i uint64, w uint8) uint64 {
	off := i * uint64(w)
	v := binary.LittleEndian.Uint64(data[off>>3:])
	return (v >> (off & 7)) & ((uint64(1) << w) - 1)
}

func setBits(data []byte, i uint64, w uint8, v uint64) {
	off := i * uint64(w)
	b := off >> 3
	sh := off & 7
	cur := binary.LittleEndian.Uint64(data[b:])
	cur &^= ((uint64(1) << w) - 1) << sh
	cur |= v << sh
	binary.LittleEndian.PutUint64(data[b:], cur)
}

// Slot returns the position of key hash x in [0, N). It is only meaningful for keys the table was built from.
func (t *Table) Slot(x uint64) uint64 {
	r := bits.RotateLeft64(x, 32)
	var p uint64
	if x < t.P1Thr {
		p = getBits(t.Front, Reduce(r, t.P2), t.FW)
	} else {
		p = getBits(t.Back, Reduce(r, t.NB-t.P2), t.BW)
	}
	q := Reduce(Mix(x^HP(p)), t.M)
	if q >= t.N {
		q = uint64(binary.LittleEndian.Uint32(t.Free[(q-t.N)*4:]))
	}
	return q
}

// PackedSize is the byte length of a pilot section holding count entries of width w (8-aligned + 8 padding).
func PackedSize(count uint64, w uint8) uint64 {
	return (count*uint64(w)+63)/64*8 + 8
}

// FreeSize is the byte length of the free-slot section (8-aligned).
func FreeSize(n, m uint64) uint64 { return ((m-n)*4 + 7) / 8 * 8 }

func bucketOf(x, nb, p2, p1thr uint64) uint64 {
	r := bits.RotateLeft64(x, 32)
	if x < p1thr {
		return Reduce(r, p2)
	}
	return p2 + Reduce(r, nb-p2)
}

func widthFor(v uint64) uint8 {
	if v == 0 {
		return 1
	}
	return uint8(bits.Len64(v))
}

// Build constructs a minimal perfect hash over xs (distinct 64-bit key hashes).
func Build(xs []uint64) (*Table, error) { return BuildWith(xs, DefaultC, DefaultAlpha) }

func BuildWith(xs []uint64, c, alpha float64) (*Table, error) {
	n := uint64(len(xs))
	if n == 0 {
		return nil, ErrNoKeys
	}
	m := uint64(math.Ceil(float64(n) / alpha))
	if m < n {
		m = n
	}
	ln := n
	if ln < 2 {
		ln = 2
	}
	nb := uint64(math.Ceil(c * float64(n) / math.Log2(float64(ln))))
	if nb < 2 {
		nb = 2
	}
	p2 := uint64(0.3 * float64(nb))
	if p2 < 1 {
		p2 = 1
	}
	p1thr := uint64(0.6 * float64(math.MaxUint64))
	t := &Table{N: n, M: m, NB: nb, P2: p2, P1Thr: p1thr}

	cnt := make([]uint32, nb+1)
	for _, x := range xs {
		cnt[bucketOf(x, nb, p2, p1thr)+1]++
	}
	for i := uint64(0); i < nb; i++ {
		cnt[i+1] += cnt[i]
	}
	grp := make([]uint64, n)
	pos := append([]uint32(nil), cnt[:nb]...)
	for _, x := range xs {
		b := bucketOf(x, nb, p2, p1thr)
		grp[pos[b]] = x
		pos[b]++
	}
	pos = nil

	maxSz := uint32(0)
	for b := uint64(0); b < nb; b++ {
		if s := cnt[b+1] - cnt[b]; s > maxSz {
			maxSz = s
		}
	}
	bySize := make([][]uint32, maxSz+1)
	for b := uint64(0); b < nb; b++ {
		s := cnt[b+1] - cnt[b]
		bySize[s] = append(bySize[s], uint32(b))
	}

	taken := make([]uint64, (m+63)/64)
	pilots := make([]uint32, nb)
	posBuf := make([]uint64, 0, maxSz)
	for s := int(maxSz); s >= 1; s-- {
		for _, b := range bySize[s] {
			ks := grp[cnt[b]:cnt[b+1]]
			for i := 1; i < len(ks); i++ {
				for j := 0; j < i; j++ {
					if ks[i] == ks[j] {
						return nil, ErrDuplicateHash
					}
				}
			}
			found := false
			for p := uint64(0); p <= math.MaxUint32; p++ {
				h := HP(p)
				posBuf = posBuf[:0]
				ok := true
				for _, x := range ks {
					q := Reduce(Mix(x^h), m)
					if taken[q>>6]>>(q&63)&1 != 0 {
						ok = false
						break
					}
					for _, e := range posBuf {
						if e == q {
							ok = false
							break
						}
					}
					if !ok {
						break
					}
					posBuf = append(posBuf, q)
				}
				if ok {
					for _, q := range posBuf {
						taken[q>>6] |= 1 << (q & 63)
					}
					pilots[b] = uint32(p)
					found = true
					break
				}
			}
			if !found {
				return nil, errors.New("pthash: no pilot found")
			}
		}
	}

	var mf, mb uint32
	for b := uint64(0); b < nb; b++ {
		if b < p2 {
			if pilots[b] > mf {
				mf = pilots[b]
			}
		} else if pilots[b] > mb {
			mb = pilots[b]
		}
	}
	t.FW, t.BW = widthFor(uint64(mf)), widthFor(uint64(mb))
	t.Front = make([]byte, PackedSize(p2, t.FW))
	t.Back = make([]byte, PackedSize(nb-p2, t.BW))
	for b := uint64(0); b < nb; b++ {
		if b < p2 {
			setBits(t.Front, b, t.FW, uint64(pilots[b]))
		} else {
			setBits(t.Back, b-p2, t.BW, uint64(pilots[b]))
		}
	}

	t.Free = make([]byte, FreeSize(n, m))
	if m > n {
		var free []uint32
		for q := uint64(0); q < n; q++ {
			if taken[q>>6]>>(q&63)&1 == 0 {
				free = append(free, uint32(q))
			}
		}
		fi := 0
		for q := n; q < m; q++ {
			if taken[q>>6]>>(q&63)&1 != 0 {
				binary.LittleEndian.PutUint32(t.Free[(q-n)*4:], free[fi])
				fi++
			}
		}
	}
	return t, nil
}
