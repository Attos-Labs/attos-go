package attos

import (
	"encoding/binary"
	"sync"

	"github.com/Attos-Labs/attos-go/internal/lz4"
)

var scratchPool = sync.Pool{New: func() any { b := make([]byte, 0, 4096); return &b }}

const (
	blockStored = 0
	blockLZ4    = 1
	maxBlockRaw = 1 << 28
)

// blockBytes returns the bytes of block b (method byte included), bounds checked.
func (m *mapping) blockBytes(b uint64) ([]byte, error) {
	o := m.offsetsOff + int(b)*4
	if o+8 > len(m.data) {
		return nil, ErrCorruptedBinary
	}
	start := binary.LittleEndian.Uint32(m.data[o:])
	end := binary.LittleEndian.Uint32(m.data[o+4:])
	s, e := m.d+int(start), m.d+int(end)
	if end <= start || e > len(m.data)-4 {
		return nil, ErrCorruptedBinary
	}
	return m.data[s:e], nil
}

// decodeBlock returns the uncompressed block content: for each record, uvarint(len) | record.
// Stored blocks alias the mapping; LZ4 blocks are decoded into a new buffer.
func decodeBlock(blk []byte, scratch *[]byte) ([]byte, error) {
	rawLen, n := binary.Uvarint(blk[1:])
	if n <= 0 || rawLen > maxBlockRaw {
		return nil, ErrCorruptedBinary
	}
	payload := blk[1+n:]
	switch blk[0] {
	case blockStored:
		if uint64(len(payload)) != rawLen {
			return nil, ErrCorruptedBinary
		}
		return payload, nil
	case blockLZ4:
		if rawLen > uint64(len(payload))*255+16 {
			return nil, ErrCorruptedBinary
		}
		var raw []byte
		if scratch != nil && uint64(cap(*scratch)) >= rawLen+lz4.Slack {
			raw = (*scratch)[:rawLen]
		} else {
			raw = make([]byte, rawLen, rawLen+lz4.Slack)
			if scratch != nil && rawLen <= 1<<20 {
				*scratch = raw[: 0 : rawLen+lz4.Slack]
			}
		}
		if err := lz4.Decompress(raw, payload); err != nil {
			return nil, ErrCorruptedBinary
		}
		return raw, nil
	}
	return nil, ErrCorruptedBinary
}

// nextRecord splits one uvarint-length-prefixed record off the front of raw.
func nextRecord(raw []byte) (rec, rest []byte, ok bool) {
	l, n := binary.Uvarint(raw)
	if n <= 0 || l > uint64(len(raw)-n) {
		return nil, nil, false
	}
	return raw[n : n+int(l)], raw[n+int(l):], true
}

// recordCompressed locates the record for keyHash inside its block. Copying the record out means
// compressed datasets allocate on every lookup; see FORMAT_V2.md for the latency trade-off.
func (m *mapping) recordCompressed(keyHash uint64) ([]byte, error) {
	slot := m.table.Slot(keyHash)
	if slot >= m.table.N {
		return nil, ErrKeyNotFound
	}
	blk, err := m.blockBytes(slot >> m.blockShift)
	if err != nil {
		return nil, err
	}
	sp := scratchPool.Get().(*[]byte)
	defer scratchPool.Put(sp)
	raw, err := decodeBlock(blk, sp)
	if err != nil {
		return nil, err
	}
	var rec []byte
	for i := slot & (1<<m.blockShift - 1); ; i-- {
		var ok bool
		rec, raw, ok = nextRecord(raw)
		if !ok {
			return nil, ErrCorruptedBinary
		}
		if i == 0 {
			break
		}
	}
	// rec aliases either the mapping (stored block) or a pooled scratch buffer: always hand out a copy.
	return append([]byte(nil), rec...), nil
}

// scanCompressed iterates every record, one block at a time.
func (m *mapping) scanCompressed(fn func(key, value []byte) bool) {
	per := uint64(1) << m.blockShift
	nBlocks := (m.totalKeys + per - 1) / per
	var scanScratch []byte
	for b := uint64(0); b < nBlocks; b++ {
		blk, err := m.blockBytes(b)
		if err != nil {
			return
		}
		raw, err := decodeBlock(blk, &scanScratch)
		if err != nil {
			return
		}
		count := per
		if rem := m.totalKeys - b*per; rem < count {
			count = rem
		}
		for i := uint64(0); i < count; i++ {
			var rec []byte
			var ok bool
			rec, raw, ok = nextRecord(raw)
			if !ok || len(rec) < 2 {
				return
			}
			keyLen := int(binary.BigEndian.Uint16(rec))
			if len(rec) < 2+keyLen {
				return
			}
			if !fn(rec[2:2+keyLen], rec[2+keyLen:]) {
				return
			}
		}
	}
}

// scanV2 iterates every record of a v2 file in slot order.
func (mp *mapping) scanV2(fn func(key, value []byte) bool) {
	if mp.compressed {
		mp.scanCompressed(fn)
		return
	}
	for i := uint64(0); i < mp.totalKeys; i++ {
		o := mp.offsetsOff + int(i)*4
		start := binary.LittleEndian.Uint32(mp.data[o:])
		end := binary.LittleEndian.Uint32(mp.data[o+4:])
		if end <= start {
			continue
		}
		s, stop := mp.d+int(start), mp.d+int(end)
		if stop > mp.end || stop-s < 2 {
			continue
		}
		e := mp.data[s:stop]
		kl := int(binary.BigEndian.Uint16(e))
		if 2+kl > len(e) {
			continue
		}
		if !fn(e[2:2+kl], e[2+kl:]) {
			return
		}
	}
}
