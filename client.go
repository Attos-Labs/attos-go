package attos

import (
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"
)

const defaultBaseURL = "http://localhost:8080"

// ─────────────────────────────────────────────────────────────
// mmapDB holds the entire .nh blob in a single mmap region.
// All lookups are pure pointer arithmetic — zero syscalls,
// zero allocations, zero locks.
// ─────────────────────────────────────────────────────────────

type mmapDB struct {
	data []byte // full file mmap'd read-only

	// State (deserialized from mmap)
	mapSalt  uint64
	mapBits  []bitVec  // bitvectors per level
	mapRanks []uint64  // pre-computed cumulative ranks

	// Offset table (slice into mmap — no copy)
	offset []uint64 // [i]=uint32 signature | uint32 file offset
	nkeys  uint64
	flags  uint32
}

// bitVec is a read-only bitvector backed by mmap'd memory.
// No mutex needed — the data is immutable after construction.
// rankIdx provides O(1) rank queries via a pre-computed prefix-sum.
type bitVec struct {
	v       []uint64 // bitvector words (from mmap)
	rankIdx []uint32 // rankIdx[i] = popcount of words 0..(i*4)-1 (1 index per 4 words)
}

func (b *bitVec) size() uint64 {
	return uint64(len(b.v)) * 64
}

func (b *bitVec) isSet(i uint64) bool {
	return 1 == (1 & (b.v[i/64] >> (i % 64)))
}

// rank returns the number of set bits before position i.
// O(1) via pre-computed prefix-sum index (sampled every 4 words).
func (b *bitVec) rank(i uint64) uint64 {
	wordIdx := i / 64
	blockIdx := wordIdx / 4
	
	// Start with the prefix sum of all blocks before this one
	r := uint64(b.rankIdx[blockIdx])
	
	// Sum the popcounts of the words leading up to our target word
	startWord := blockIdx * 4
	for j := startWord; j < wordIdx; j++ {
		r += uint64(bits.OnesCount64(b.v[j]))
	}
	
	// Add the partial popcount of the target word
	y := i % 64
	mask := (uint64(1) << y) - 1
	r += uint64(bits.OnesCount64(b.v[wordIdx] & mask))
	
	return r
}

// buildRankIndex pre-computes the prefix-sum table for O(1) rank.
// Samples every 4 words (256 bits) to drastically cut memory overhead.
func (b *bitVec) buildRankIndex() {
	n := len(b.v)
	idxLen := (n+3)/4 + 1
	b.rankIdx = make([]uint32, idxLen) // cast to uint32 saves 50%
	var cumulative uint64
	for i := 0; i < n; i++ {
		if i%4 == 0 {
			b.rankIdx[i/4] = uint32(cumulative)
		}
		cumulative += uint64(bits.OnesCount64(b.v[i]))
	}
	b.rankIdx[idxLen-1] = uint32(cumulative)
}

func (b *bitVec) popcount() uint64 {
	// If rankIdx is built, total popcount is the last element
	if len(b.rankIdx) > 0 {
		return uint64(b.rankIdx[len(b.rankIdx)-1])
	}
	var p uint64
	for _, w := range b.v {
		p += uint64(bits.OnesCount64(w))
	}
	return p
}

// bhash: binary protocol block hash
func bhash(key, salt uint64, lvl uint32) uint64 {
	const m uint64 = 0x880355f21e6d1965
	h := m
	h ^= mix(key)
	h *= m
	h ^= mix(salt)
	h *= m
	h ^= mix(uint64(lvl))
	h *= m
	h = mix(h)
	return h
}

// mix: compression function for hash
func mix(h uint64) uint64 {
	h ^= h >> 23
	h *= 0x2127599bf4325c37
	h ^= h >> 47
	return h
}

// find performs the ZeroLatencyMap lookup — pure arithmetic on mmap'd bitvectors.
func (m *mmapDB) find(key uint64) (uint64, bool) {
	for lvl, bv := range m.mapBits {
		i := bhash(key, m.mapSalt, uint32(lvl)) % bv.size()
		if !bv.isSet(i) {
			continue
		}
		// compute a 1-based rank and return a 0-based index
		rank := 1 + m.mapRanks[lvl] + bv.rank(i)
		return rank - 1, true
	}
	return 0, false
}

func (m *mmapDB) get(keyHash uint64) (string, bool) {
	idx, ok := m.find(keyHash)
	if !ok {
		return "", false
	}

	if idx+1 >= uint64(len(m.offset)) {
		return "", false
	}

	entry := m.offset[idx]
	sig := uint32(entry >> 32)
	valOff := uint32(entry)

	// Verify hash signature match (false positive check)
	expectedSig := uint32(keyHash >> 32)
	if sig != expectedSig {
		return "", false
	}

	// Because values are written strictly sequentially by rank,
	// length is the difference to the next offset.
	nextEntry := m.offset[idx+1]
	nextValOff := uint32(nextEntry)
	
	valStart := uint64(valOff)
	valEnd := uint64(nextValOff)

	if valEnd > uint64(len(m.data)) || valStart > valEnd {
		return "", false
	}

	val := m.data[valStart:valEnd]
	if len(val) == 0 {
		return "", true
	}

	// Zero-allocation byte→string: mmap is read-only, safe to alias
	return unsafe.String(unsafe.SliceData(val), len(val)), true
}

// ─────────────────────────────────────────────────────────────
// Client is the Attos data-plane SDK for syncing and querying binary blobs locally.
// ─────────────────────────────────────────────────────────────

type Client struct {
	apiKey    string
	baseURL   string
	cacheDir  string
	datasetID string
	blobPath  string
	db        atomic.Pointer[mmapDB]
}

// Option configures the client.
type Option func(*Client)

// WithBaseURL sets the control plane URL.
func WithBaseURL(url string) Option {
	return func(c *Client) { c.baseURL = url }
}

// WithCacheDir sets where .nh blobs are stored locally.
func WithCacheDir(dir string) Option {
	return func(c *Client) { c.cacheDir = dir }
}

// NewClient creates a Attos SDK client authenticated with the given API key.
func NewClient(apiKey string, opts ...Option) *Client {
	c := &Client{
		apiKey:   apiKey,
		baseURL:  defaultBaseURL,
		cacheDir: "./.attos/cache",
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Sync downloads the compiled .nh blob for datasetID and saves it to local disk.
// After sync, the blob is memory-mapped for O(1) lookups via Get.
func (c *Client) Sync(datasetID string) error {
	url := fmt.Sprintf("%s/api/v1/sync/%s", c.baseURL, datasetID)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("sync request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sync failed (%d): %s", resp.StatusCode, string(body))
	}

	if err := os.MkdirAll(c.cacheDir, 0o755); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}

	blobPath := filepath.Join(c.cacheDir, datasetID+".nh")
	f, err := os.Create(blobPath)
	if err != nil {
		return fmt.Errorf("create blob file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("write blob: %w", err)
	}

	return c.openBlob(datasetID, blobPath)
}

// openBlob memory-maps the entire .nh file and deserializes the map index
// directly from the mmap'd bytes — zero copies, zero disk I/O on read.
func (c *Client) openBlob(datasetID, blobPath string) error {
	db, err := openMmapDB(blobPath)
	if err != nil {
		return fmt.Errorf("open mmap db: %w", err)
	}

	c.datasetID = datasetID
	c.blobPath = blobPath

	oldDB := c.db.Swap(db)
	if oldDB != nil {
		munmapDB(oldDB)
	}

	return nil
}

// Get performs an O(1) local lookup against the memory-mapped blob.
// This function is zero-allocation and lock-free — safe for unlimited concurrent use.
func (c *Client) Get(key string) (string, error) {
	db := c.db.Load()
	if db == nil {
		return "", fmt.Errorf("no blob loaded; call Sync() first")
	}

	val, ok := db.get(hashKey(key))
	if !ok {
		return "", fmt.Errorf("key not found")
	}
	return val, nil
}

func (c *Client) Close() error {
	db := c.db.Swap(nil)
	if db != nil {
		munmapDB(db)
	}
	return nil
}

// BlobPath returns the local path to the synced .nh file.
func (c *Client) BlobPath() string {
	return c.blobPath
}

// hashKey implements a deterministic 64-bit FNV-1a hash.
// Identical to the backend compile.hashKey — they MUST match.
func hashKey(key string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= 1099511628211
	}
	return h
}

// ─────────────────────────────────────────────────────────────
// mmap DB loader: parses the .nh binary format and sets up
// direct pointers into the mmap'd region. After this, all
// lookups are pure memory operations.
// ─────────────────────────────────────────────────────────────

// File format constants
const (
	dbKeysOnly  = 1 << iota
	magicZeroLatency = "MPHB"
	magicLegacy    = "MPHC"
)

func openMmapDB(path string) (*mmapDB, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	sz := info.Size()
	if sz < 64+32 {
		return nil, fmt.Errorf("%s: file too small (%d bytes)", path, sz)
	}

	// Mmap the entire file read-only
	data, err := syscall.Mmap(int(f.Fd()), 0, int(sz), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap: %w", err)
	}

	db := &mmapDB{data: data}

	// Parse 64-byte header (big-endian)
	be := binary.BigEndian
	magic := string(data[:4])
	if magic != magicZeroLatency && magic != magicLegacy {
		syscall.Munmap(data)
		return nil, fmt.Errorf("bad magic: %q", magic)
	}
	if magic == magicLegacy {
		syscall.Munmap(data)
		return nil, fmt.Errorf("Legacy format not supported in zero-copy mode")
	}

	db.flags = be.Uint32(data[4:8])
	// salt is at bytes 8..24 (16 bytes, used by siphash for record checksums — we skip those)
	db.nkeys = be.Uint64(data[24:32])
	offtbl := be.Uint64(data[32:40])

	if offtbl < 64 || offtbl >= uint64(sz)-32 {
		syscall.Munmap(data)
		return nil, fmt.Errorf("corrupt header: offtbl=%d, size=%d", offtbl, sz)
	}

	// The mmap'd region from offtbl onward contains:
	//   [offset table] [map index bytes] ... [32-byte SHA trailer]
	meta := data[offtbl : uint64(sz)-32]

	// Keys+Values DB: offset table is [uint32 signature | uint32 fileOffset] per key + 1 dummy
	offsz := (db.nkeys + 1) * 8
	db.offset = bytesToUint64Slice(meta[:offsz])
	meta = meta[offsz:]

	// Align to 8-byte boundary.
	// aligns data to 8-byte file offsets. Calculate the
	// file offset of where we currently are in meta, then skip padding.
	metaConsumed := offsz
	fileOff := offtbl + metaConsumed
	if rem := fileOff % 8; rem != 0 {
		skip := 8 - rem
		if skip <= uint64(len(meta)) {
			meta = meta[skip:]
		}
	}

	// Deserialize map index from mmap'd bytes
	if err := db.deserializeZeroLatencyMap(meta); err != nil {
		syscall.Munmap(data)
		return nil, fmt.Errorf("deserialize map: %w", err)
	}

	// Warm up: pre-fault all pages into physical memory
	warmUp(data)

	return db, nil
}

// deserializeZeroLatencyMap reads the header and bitvectors directly from mmap'd bytes.
// Operates on our mmap'd region.
func (db *mmapDB) deserializeZeroLatencyMap(buf []byte) error {
	if len(buf) < 16 {
		return fmt.Errorf("header too short: %d bytes", len(buf))
	}

	le := binary.LittleEndian
	ver := buf[0]
	if ver != 1 {
		return fmt.Errorf("unsupported version: %d", ver)
	}

	nlevels := le.Uint32(buf[4:8])
	db.mapSalt = le.Uint64(buf[8:16])

	if nlevels == 0 || nlevels > 4000 {
		return fmt.Errorf("invalid levels: %d", nlevels)
	}

	buf = buf[16:]
	db.mapBits = make([]bitVec, nlevels)

	for i := uint32(0); i < nlevels; i++ {
		if len(buf) < 8 {
			return fmt.Errorf("level %d: truncated", i)
		}
		bvWords := le.Uint64(buf[:8])
		if bvWords == 0 || bvWords > (1<<32) {
			return fmt.Errorf("level %d: invalid bitvector length %d", i, bvWords)
		}

		byteLen := bvWords * 8
		if uint64(len(buf)-8) < byteLen {
			return fmt.Errorf("level %d: bitvector data truncated", i)
		}

		// Zero-copy: point directly into mmap'd memory
		db.mapBits[i] = bitVec{
			v: bytesToUint64Slice(buf[8 : 8+byteLen]),
		}
		// Build O(1) rank index for this bitvector
		db.mapBits[i].buildRankIndex()
		buf = buf[8+byteLen:]
	}

	// Pre-compute cumulative ranks
	db.mapRanks = make([]uint64, nlevels)
	var pop uint64
	for l := range db.mapBits {
		db.mapRanks[l] = pop
		pop += db.mapBits[l].popcount()
	}

	return nil
}

// warmUp forces the OS to page-in the entire mmap'd region by
// touching every 4KB page. This prevents page-fault latency spikes
// during hot-path lookups.
func warmUp(data []byte) {
	const pageSize = 4096
	var dummy byte
	for i := 0; i < len(data); i += pageSize {
		dummy += data[i]
	}
	// Last byte
	if len(data) > 0 {
		dummy += data[len(data)-1]
	}
	runtime.KeepAlive(dummy)
}

func munmapDB(db *mmapDB) {
	if db != nil && db.data != nil {
		syscall.Munmap(db.data)
		db.data = nil
	}
}

// ─────────────────────────────────────────────────────────────
// Zero-copy slice reinterpret casts (matches go-mph's slices.go)
// ─────────────────────────────────────────────────────────────

func bytesToUint64Slice(b []byte) []uint64 {
	n := len(b) / 8
	if n == 0 {
		return nil
	}
	return unsafe.Slice((*uint64)(unsafe.Pointer(unsafe.SliceData(b))), n)
}

func bytesToUint32Slice(b []byte) []uint32 {
	n := len(b) / 4
	if n == 0 {
		return nil
	}
	return unsafe.Slice((*uint32)(unsafe.Pointer(unsafe.SliceData(b))), n)
}

// MmapFile maps a file read-only using syscall.Mmap (for production use).
func MmapFile(path string) ([]byte, error) {
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
