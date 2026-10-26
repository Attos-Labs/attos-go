package attos

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/Attos-Labs/attos-go/internal/mmap"
	"github.com/Attos-Labs/attos-go/internal/pthash"
	"github.com/cespare/xxhash/v2"
)

// ---------------------------------------------------------------------------
// Hot-swap and retirement scheme (CONTRACTS.md §4)
//
// A Map is a handle to one immutable *mapping (an mmap'd, validated blob)
// published through an atomic.Pointer. Reload / the Synchronizer replace it
// with a single atomic store; readers never observe a partially valid map.
//
// Reclamation uses per-mapping striped reader counters (a Dekker-style
// handshake, i.e. a hazard counter rather than an epoch):
//
//	reader:  mp := cur.Load(); stripe.enter.Add(1); if mp.retired.Load() { back off, retry }
//	         ... read mp.data ...; stripe.exit.Add(1)
//	writer:  old := cur.Swap(new); old.retired.Store(true)
//	         wait until Σ exit == Σ enter over all stripes; sleep RetireGrace; munmap
//
// Go's sync/atomic operations are sequentially consistent, so either the
// reader's load of `retired` observes true (it backs off without touching the
// mapped bytes and retries on the new pointer) or the writer's read of the
// stripe counters observes the reader's enter (it waits). A lookup in progress
// therefore always completes against the mapping it loaded, and no mapping is
// unmapped while a lookup can still touch it (no use-after-unmap).
//
// Stripes are chosen from the key hash so concurrent readers rarely share a
// cache line; the hot path is two uncontended atomic adds and two atomic
// loads — no locks, no allocation, no syscalls. The monotonic exit counters
// double as the lookup counter used by telemetry.
//
// Bounds: at most maxRetired (2) mappings are pending retirement at once;
// a third swap blocks until one is released, so retired maps cannot grow
// without bound. File descriptors are closed right after mmap, so retirement
// never leaks fds.
//
// Zero-copy validity window: a slice/string returned by Get/GetString aliases
// the mapping. It stays valid while that mapping is current, and for at least
// OpenConfig.RetireGrace (default 2 s) after it is replaced. After Close it is
// invalid. Use GetCopy (copies into a caller buffer, 0 allocs with capacity)
// or View (callback runs while the mapping is pinned) for anything longer.
// ---------------------------------------------------------------------------

const (
	readerStripes = 32
	stripeMask    = readerStripes - 1
	maxRetired    = 2
)

type stripe struct {
	enter atomic.Uint64
	exit  atomic.Uint64
	miss  atomic.Uint64 // lookups that found no value
	_     [40]byte      // pad to a 64-byte cache line
}

// mapping is one immutable, validated, mmap'd blob.
type mapping struct {
	data    []byte
	is32    bool
	slots   uint64 // S
	buckets uint64 // B
	r, d    int    // routing-table end / data-block start offsets
	end     int    // len(data) - 4 (start of CRC trailer)
	pinned  bool

	// Version 2 (PTHash) state: slot table, uint32 offset table (n+1 entries), blob.
	version    uint32
	table      *pthash.Table
	offsetsOff int
	offW       int   // bytes per offset: 4 or 5
	lean       bool  // flags bit 1: records are fingerprint|value, keys are not stored
	fpW        int   // fingerprint bytes when lean (4 or 8)
	compressed bool  // flags bit 0: records live in LZ4/stored blocks
	blockShift uint8 // log2(records per block) when compressed
	totalKeys  uint64

	size int64 // len(data); immutable, readable without pinning

	retired    atomic.Bool
	readers    [readerStripes]stripe
	nonLookups atomic.Uint64 // pinned operations that are not lookups (Len, Scan)
	batchExtra atomic.Uint64 // lookups served beyond the one counted per pin (GetMany)

	rowsOnce sync.Once
	rows     int
}

func (mp *mapping) unmap() error {
	if mp.pinned {
		_ = mmap.Munlock(mp.data)
	}
	err := mmap.Unmap(mp.data)
	mp.data = nil
	return err
}

func (mp *mapping) inFlight() bool {
	var en, ex uint64
	for i := range mp.readers {
		ex += mp.readers[i].exit.Load()
	}
	for i := range mp.readers {
		en += mp.readers[i].enter.Load()
	}
	return en != ex
}

func (mp *mapping) lookups() uint64 {
	var n uint64
	for i := range mp.readers {
		n += mp.readers[i].exit.Load()
	}
	return n - mp.nonLookups.Load() + mp.batchExtra.Load()
}

func (mp *mapping) misses() uint64 {
	var n uint64
	for i := range mp.readers {
		n += mp.readers[i].miss.Load()
	}
	return n
}

// waitDrained spins (with backoff) until no reader is inside mp.
func (mp *mapping) waitDrained() {
	for d := time.Microsecond; mp.inFlight(); {
		time.Sleep(d)
		if d < 10*time.Millisecond {
			d *= 2
		}
	}
}

// entry resolves key (with precomputed hash h) to its value. CONTRACTS §1.6.
func (mp *mapping) entry(h uint64, key []byte) ([]byte, error) {
	if mp.version == 2 {
		return mp.entryV2(h, key)
	}
	data := mp.data
	be := binary.BigEndian
	bk := uint64(uint32(h>>32)) % mp.buckets
	var d uint64
	if mp.is32 {
		d = uint64(be.Uint32(data[uint64(headerSize)+bk*4:]))
	} else {
		d = uint64(be.Uint16(data[uint64(headerSize)+bk*2:]))
	}
	slot := (uint64(uint32(h)) ^ d) % mp.slots
	word := be.Uint64(data[uint64(mp.r)+slot*8:])
	ln := word & 0xFFFFFFFF
	if ln == 0 {
		return nil, ErrKeyNotFound
	}
	start := uint64(mp.d) + word>>32
	stop := start + ln
	if ln < 2 || stop > uint64(mp.end) {
		return nil, ErrCorruptedBinary
	}
	e := data[start:stop]
	kl := uint64(be.Uint16(e))
	if 2+kl > ln {
		return nil, ErrCorruptedBinary
	}
	if string(e[2:2+kl]) != string(key) { // compiles to memequal, no allocation
		return nil, ErrKeyNotFound
	}
	return e[2+kl:], nil
}

// off returns offset i of the v2 offsets section (4 or 5 bytes, little endian).
func (mp *mapping) off(i uint64) uint64 {
	if mp.offW == 5 {
		return binary.LittleEndian.Uint64(mp.data[mp.offsetsOff+int(i)*5:]) & 0xFFFFFFFFFF
	}
	return uint64(binary.LittleEndian.Uint32(mp.data[mp.offsetsOff+int(i)*4:]))
}

// entryV2 resolves key through the PTHash table. Every hash maps to some slot, so the stored key is verified.
func (mp *mapping) entryV2(h uint64, key []byte) ([]byte, error) {
	var e []byte
	if mp.compressed {
		var err error
		if e, err = mp.recordCompressed(h); err != nil {
			return nil, err
		}
	} else {
		slot := mp.table.Slot(h)
		if slot >= mp.table.N { // only possible with a corrupt free-slot table
			return nil, ErrKeyNotFound
		}
		start, end := mp.off(slot), mp.off(slot+1)
		if end <= start {
			return nil, ErrKeyNotFound
		}
		s, stop := mp.d+int(start), mp.d+int(end)
		if stop > mp.end {
			return nil, ErrCorruptedBinary
		}
		e = mp.data[s:stop]
	}
	if mp.lean {
		if len(e) < mp.fpW {
			return nil, ErrCorruptedBinary
		}
		fp := keyFingerprint(key)
		if string(e[:mp.fpW]) != string(fp[:mp.fpW]) {
			return nil, ErrKeyNotFound
		}
		return e[mp.fpW:], nil
	}
	if len(e) < 2 {
		return nil, ErrCorruptedBinary
	}
	kl := int(binary.BigEndian.Uint16(e))
	if 2+kl > len(e) {
		return nil, ErrCorruptedBinary
	}
	if string(e[2:2+kl]) != string(key) {
		return nil, ErrKeyNotFound
	}
	return e[2+kl:], nil
}

func (mp *mapping) countRows() int {
	if mp.version == 2 {
		return int(mp.totalKeys)
	}
	mp.rowsOnce.Do(func() {
		be := binary.BigEndian
		n := 0
		for off := mp.r; off < mp.d; off += 8 {
			if be.Uint64(mp.data[off:])&0xFFFFFFFF != 0 {
				n++
			}
		}
		mp.rows = n
	})
	return mp.rows
}

// Map is a read-only, memory-mapped Attos blob that can be hot-swapped.
// All methods are safe for concurrent use. Lookups are lock-free and
// allocation-free.
type Map struct {
	cur    atomic.Pointer[mapping]
	closed atomic.Bool
	cfg    OpenConfig

	retireMu   sync.Mutex
	retireCond *sync.Cond
	pending    int           // mappings swapped out but not yet unmapped
	shutdown   chan struct{} // closed by Close to cut retire grace short
	retireWG   sync.WaitGroup

	retiredLookups atomic.Uint64 // lookups served by already-unmapped mappings
	retiredMisses  atomic.Uint64
	swaps          atomic.Uint64
	closeOnce      sync.Once
	closeErr       error

	lat latencyHistogram
}

// newEmptyMap returns a handle with no mapping yet (used by Synchronizer).
func newEmptyMap(cfg OpenConfig) *Map {
	return &Map{cfg: cfg}
}

func (m *Map) init() {
	m.retireMu.Lock()
	if m.retireCond == nil {
		m.retireCond = sync.NewCond(&m.retireMu)
		m.shutdown = make(chan struct{})
	}
	m.retireMu.Unlock()
}

// enter pins the current mapping for a read. It returns nil if the Map is
// closed (or has no mapping yet). The caller must call exit(mp, s).
func (m *Map) enter(h uint64) (*mapping, *stripe, uint64) {
	for {
		mp := m.cur.Load()
		if mp == nil {
			return nil, nil, 0
		}
		s := &mp.readers[h&stripeMask]
		n := s.enter.Add(1)
		if !mp.retired.Load() {
			return mp, s, n
		}
		s.enter.Add(^uint64(0)) // back off; mp is being retired
	}
}

// noteMiss counts a lookup that found no value.
func (s *stripe) noteMiss(err error) {
	if err == ErrKeyNotFound {
		s.miss.Add(1)
	}
}

func (m *Map) noMapErr() error {
	if m.closed.Load() {
		return ErrMapClosed
	}
	return ErrNoMap
}

// sampleMask selects 1 in 256 lookups (per stripe) for latency sampling.
const sampleMask = 255

// Get performs an O(1) lookup and returns a zero-copy slice of the value.
// See the package comment above for the validity window; use GetCopy or View
// to keep the value longer. Zero allocations.
func (m *Map) Get(key []byte) ([]byte, error) {
	h := xxhash.Sum64(key)
	mp, s, n := m.enter(h)
	if mp == nil {
		return nil, m.noMapErr()
	}
	if n&sampleMask == 0 {
		v, err := m.entryTimed(mp, h, key)
		s.noteMiss(err)
		s.exit.Add(1)
		return v, err
	}
	v, err := mp.entry(h, key)
	s.noteMiss(err)
	s.exit.Add(1)
	return v, err
}

// entryTimed resolves key and records its latency for telemetry. Every lookup
// accessor (Get, GetString, GetCopy, View) calls it for 1 in 256 lookups (per
// stripe, chosen by the enter counter value), so the telemetry percentiles
// reflect whichever accessor the application uses. The unsampled fast path
// calls mp.entry directly in each accessor.
func (m *Map) entryTimed(mp *mapping, h uint64, key []byte) ([]byte, error) {
	t0 := time.Now()
	v, err := mp.entry(h, key)
	m.lat.record(time.Since(t0))
	return v, err
}

// GetString performs an O(1) lookup returning a zero-copy string with the same
// validity window as Get. Zero allocations.
func (m *Map) GetString(key string) (string, error) {
	kb := unsafe.Slice(unsafe.StringData(key), len(key))
	v, err := m.Get(kb)
	if err != nil {
		return "", err
	}
	if len(v) == 0 {
		return "", nil
	}
	return unsafe.String(unsafe.SliceData(v), len(v)), nil
}

// GetCopy looks up key and appends its value to dst, returning the extended
// slice. The result never aliases the mapping and stays valid across swaps and
// Close. Zero allocations when cap(dst) is large enough.
func (m *Map) GetCopy(key, dst []byte) ([]byte, error) {
	h := xxhash.Sum64(key)
	mp, s, n := m.enter(h)
	if mp == nil {
		return dst, m.noMapErr()
	}
	var v []byte
	var err error
	if n&sampleMask == 0 {
		v, err = m.entryTimed(mp, h, key)
	} else {
		v, err = mp.entry(h, key)
	}
	if err == nil {
		dst = append(dst, v...)
	}
	s.noteMiss(err)
	s.exit.Add(1)
	return dst, err
}

// View looks up key and calls fn with a zero-copy value. The mapping is pinned
// for the duration of fn, so the slice is valid until fn returns (fn must not
// retain it, call Close or block on a swap).
func (m *Map) View(key []byte, fn func(value []byte)) error {
	h := xxhash.Sum64(key)
	mp, s, n := m.enter(h)
	if mp == nil {
		return m.noMapErr()
	}
	defer s.exit.Add(1)
	var v []byte
	var err error
	if n&sampleMask == 0 {
		v, err = m.entryTimed(mp, h, key)
	} else {
		v, err = mp.entry(h, key)
	}
	if err != nil {
		s.noteMiss(err)
		return err
	}
	fn(v)
	return nil
}

// Scan iterates over all entries in slot order (arbitrary, MPHF-defined).
// The mapping is pinned for the whole scan; key and value slices are valid
// only during the callback. Return false to stop early.
func (m *Map) Scan(fn func(key, value []byte) bool) {
	mp, s, _ := m.enter(0)
	if mp == nil {
		return
	}
	mp.nonLookups.Add(1)
	defer s.exit.Add(1)
	if mp.version == 2 {
		mp.scanV2(fn)
		return
	}
	be := binary.BigEndian
	for off := mp.r; off < mp.d; off += 8 {
		word := be.Uint64(mp.data[off:])
		ln := word & 0xFFFFFFFF
		if ln == 0 {
			continue
		}
		start := uint64(mp.d) + word>>32
		stop := start + ln
		if ln < 2 || stop > uint64(mp.end) {
			continue
		}
		e := mp.data[start:stop]
		kl := uint64(be.Uint16(e))
		if 2+kl > ln {
			continue
		}
		if !fn(e[2:2+kl], e[2+kl:]) {
			return
		}
	}
}

// Len returns the number of keys in the current mapping (0 if closed).
func (m *Map) Len() int {
	mp, s, _ := m.enter(0)
	if mp == nil {
		return 0
	}
	mp.nonLookups.Add(1)
	defer s.exit.Add(1)
	return mp.countRows()
}

// Size returns the size in bytes of the currently mapped blob (0 if closed).
func (m *Map) Size() int64 {
	if mp := m.cur.Load(); mp != nil {
		return mp.size
	}
	return 0
}

// Lookups returns the total number of lookups served by this Map so far.
func (m *Map) Lookups() uint64 {
	n := m.retiredLookups.Load()
	if mp := m.cur.Load(); mp != nil {
		n += mp.lookups()
	}
	return n
}

// Misses returns the number of lookups that found no value.
// Hits are Lookups() - Misses().
func (m *Map) Misses() uint64 {
	n := m.retiredMisses.Load()
	if mp := m.cur.Load(); mp != nil {
		n += mp.misses()
	}
	return n
}

// Swaps returns how many times the mapping has been replaced.
func (m *Map) Swaps() uint64 { return m.swaps.Load() }

// Reload opens and validates path (honouring the Map's OpenConfig) and
// atomically hot-swaps it in. In-flight lookups finish on the old mapping,
// which is unmapped after they drain plus RetireGrace. Reload blocks while
// two retired mappings are still pending. On error the current map is kept.
func (m *Map) Reload(path string) error {
	mp, err := openMapping(path, m.cfg, m.cfg.ValidateChecksum)
	if err != nil {
		return err
	}
	if err := m.publish(mp); err != nil {
		mp.unmap()
		return err
	}
	return nil
}

// publish atomically installs mp and schedules retirement of the previous one.
func (m *Map) publish(mp *mapping) error {
	m.init()
	m.retireMu.Lock()
	for m.pending >= maxRetired && !m.closed.Load() {
		m.retireCond.Wait()
	}
	if m.closed.Load() {
		m.retireMu.Unlock()
		return ErrMapClosed
	}
	old := m.cur.Swap(mp)
	if old != nil {
		m.pending++
		m.swaps.Add(1)
	}
	m.retireMu.Unlock()
	if old != nil {
		old.retired.Store(true)
		m.retireWG.Add(1)
		go m.retire(old, m.cfg.retireGrace())
	}
	return nil
}

func (m *Map) retire(old *mapping, grace time.Duration) {
	defer m.retireWG.Done()
	old.waitDrained()
	if grace > 0 {
		t := time.NewTimer(grace)
		select {
		case <-t.C:
		case <-m.shutdown:
			t.Stop()
		}
	}
	m.retiredLookups.Add(old.lookups())
	m.retiredMisses.Add(old.misses())
	if err := old.unmap(); err != nil {
		m.cfg.logger().Printf("[attos] Warning: munmap of retired map failed: %v", err)
	}
	m.retireMu.Lock()
	m.pending--
	m.retireCond.Broadcast()
	m.retireMu.Unlock()
}

// PendingRetired returns the number of replaced mappings not yet unmapped.
func (m *Map) PendingRetired() int {
	m.init()
	m.retireMu.Lock()
	defer m.retireMu.Unlock()
	return m.pending
}

// Close stops all further lookups (they return ErrMapClosed), waits for
// in-flight lookups to finish and unmaps every mapping, including retired
// ones still in their grace period. Zero-copy values become invalid when
// Close returns. Close is idempotent.
func (m *Map) Close() error {
	m.closeOnce.Do(func() {
		m.init()
		m.retireMu.Lock()
		m.closed.Store(true)
		close(m.shutdown)
		old := m.cur.Swap(nil)
		m.retireCond.Broadcast()
		m.retireMu.Unlock()
		if old != nil {
			old.retired.Store(true)
			old.waitDrained()
			m.retiredLookups.Add(old.lookups())
			m.retiredMisses.Add(old.misses())
			m.closeErr = old.unmap()
		}
		m.retireWG.Wait()
	})
	return m.closeErr
}
