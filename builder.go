package attos

// Builder compiles CSV / NDJSON datasets into Attos v1 blobs.
//
// It implements CONTRACTS.md §1.1–1.5 exactly: RFC 4180 CSV via encoding/csv,
// NDJSON via encoding/json, typed errors with 1-based physical line numbers,
// integer-only sizing and the canonical deterministic placement algorithm
// (buckets ordered by size descending, bucket index ascending; smallest
// displacement first). The output is a pure function of the set of
// (key, value) pairs: it does not depend on input row order, GOMAXPROCS,
// scheduling or platform, and it is byte-identical to the golden fixtures in
// testdata/golden and to attos-cloud/engine/compiler.
//
// Memory (peak, during Compile) for n rows with K total key bytes and V total
// value bytes is approximately
//
//	K + V + 2n            (row arena, chunked in 64 MiB pieces — no doubling copies)
//	+ 24n                 (per-row offset/length/line/hash columns)
//	+ 4n                  (bucket member index)
//	+ 8·B + 4·S + S/8     (bucket offsets, displacements, slot→row, occupancy bitset)
//	+ 4 MiB               (output buffer)
//
// with S ≈ 1.2n and B ≈ 1.44n, i.e. about K + V + 60n bytes. Nothing is
// written to the destination until parsing, duplicate detection and placement
// have all succeeded.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"unsafe"

	"github.com/cespare/xxhash/v2"
)

// Format selects the input format for the Builder.
type Format int

const (
	// FormatAuto detects the format from the first non-whitespace byte:
	// '{' selects NDJSON, anything else CSV (CONTRACTS.md §1.5).
	FormatAuto Format = iota
	// FormatCSV forces RFC 4180 CSV with a header row.
	FormatCSV
	// FormatNDJSON forces newline-delimited JSON objects.
	FormatNDJSON
)

const (
	// MaxKeyLen is the largest key (in bytes) a v1 blob can store.
	MaxKeyLen = 65535
	// MaxValueLen is the largest value (in bytes) a v1 blob can store.
	MaxValueLen = 1 << 20
	// maxDataBlock is the largest data block a v1 blob can address.
	maxDataBlock = math.MaxUint32

	valueColumn = "attos_value"
	arenaChunk  = 64 << 20
)

// BuildStats describes a compiled blob.
type BuildStats struct {
	Rows            uint64 // n
	Slots           uint64 // S
	Buckets         uint64 // B
	MaxDisplacement uint32
	Bytes           int64 // bytes written, including the CRC trailer
	CRC32           uint32
}

// Builder accumulates (key, value) rows and compiles them into a v1 blob.
// A Builder is not safe for concurrent use.
type Builder struct {
	chunks [][]byte // row arena: each row is key ‖ value, never straddling chunks
	offs   []uint64 // chunk<<32 | offset within chunk
	klens  []uint16
	vlens  []uint32
	lines  []uint32
	hashes []uint64

	dataBytes uint64 // sum over rows of 2 + keyLen + valueLen
	checked   int    // rows [0, checked) are known to be duplicate-free
}

// NewBuilder returns an empty Builder.
func NewBuilder() *Builder { return &Builder{} }

// Len returns the number of rows accumulated so far.
func (b *Builder) Len() int { return len(b.offs) }

// Reset discards all accumulated rows.
func (b *Builder) Reset() { *b = Builder{} }

// BuildFromCSV parses an RFC 4180 CSV file (header row required) and adds its rows.
func (b *Builder) BuildFromCSV(csvPath string) error { return b.buildFromPath(csvPath, FormatCSV) }

// BuildFromNDJSON parses an NDJSON file and adds its rows.
func (b *Builder) BuildFromNDJSON(jsonPath string) error {
	return b.buildFromPath(jsonPath, FormatNDJSON)
}

// BuildFromFile parses a CSV or NDJSON file, detecting the format (§1.5).
func (b *Builder) BuildFromFile(path string) error { return b.buildFromPath(path, FormatAuto) }

func (b *Builder) buildFromPath(path string, format Format) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return b.BuildFrom(context.Background(), f, format)
}

// BuildFrom parses src in the given format and adds its rows. On any error the
// Builder is left exactly as it was before the call. Line numbers in errors are
// 1-based physical line numbers within src.
//
// Error precedence matches the reference implementation: a parse error anywhere
// in src wins; otherwise the earliest row that exceeds a size limit or repeats
// an earlier key is reported.
func (b *Builder) BuildFrom(ctx context.Context, src io.Reader, format Format) error {
	mark := b.mark()
	if err := b.parse(ctx, src, format); err != nil {
		b.rollback(mark)
		return err
	}
	if d := b.firstDuplicate(mark.rows); d != nil {
		b.rollback(mark)
		return d
	}
	b.checked = len(b.offs)
	return nil
}

// Add appends one row. line is used only for error reporting (use 0 if unknown).
// Add does not check for duplicates until the next BuildFrom or Compile call.
func (b *Builder) Add(key, value []byte, line int) error {
	if len(key) > MaxKeyLen {
		return &LimitError{Line: line, Field: "key", Len: len(key), Max: MaxKeyLen}
	}
	if len(value) > MaxValueLen {
		return &LimitError{Line: line, Field: "value", Len: len(value), Max: MaxValueLen}
	}
	b.appendRow(key, value, line)
	return nil
}

type builderMark struct {
	rows      int
	chunks    int
	lastLen   int
	dataBytes uint64
}

func (b *Builder) mark() builderMark {
	m := builderMark{rows: len(b.offs), chunks: len(b.chunks), dataBytes: b.dataBytes}
	if len(b.chunks) > 0 {
		m.lastLen = len(b.chunks[len(b.chunks)-1])
	}
	return m
}

func (b *Builder) rollback(m builderMark) {
	b.offs = b.offs[:m.rows]
	b.klens = b.klens[:m.rows]
	b.vlens = b.vlens[:m.rows]
	b.lines = b.lines[:m.rows]
	if len(b.hashes) > m.rows {
		b.hashes = b.hashes[:m.rows]
	}
	for i := m.chunks; i < len(b.chunks); i++ {
		b.chunks[i] = nil
	}
	b.chunks = b.chunks[:m.chunks]
	if m.chunks > 0 {
		b.chunks[m.chunks-1] = b.chunks[m.chunks-1][:m.lastLen]
	}
	b.dataBytes = m.dataBytes
	if b.checked > m.rows {
		b.checked = m.rows
	}
}

func (b *Builder) appendRow(key, value []byte, line int) {
	need := len(key) + len(value)
	if len(b.chunks) == 0 || cap(b.chunks[len(b.chunks)-1])-len(b.chunks[len(b.chunks)-1]) < need {
		size := arenaChunk
		if need > size {
			size = need
		}
		b.chunks = append(b.chunks, make([]byte, 0, size))
	}
	ci := len(b.chunks) - 1
	c := b.chunks[ci]
	off := len(c)
	c = append(c, key...)
	c = append(c, value...)
	b.chunks[ci] = c
	b.offs = append(b.offs, uint64(ci)<<32|uint64(off))
	b.klens = append(b.klens, uint16(len(key)))
	b.vlens = append(b.vlens, uint32(len(value)))
	if line < 0 || line > math.MaxUint32 {
		line = 0
	}
	b.lines = append(b.lines, uint32(line))
	b.dataBytes += uint64(2 + need)
}

func (b *Builder) rowKey(i int) []byte {
	o := b.offs[i]
	c := b.chunks[o>>32]
	s := int(uint32(o))
	return c[s : s+int(b.klens[i])]
}

func (b *Builder) rowValue(i int) []byte {
	o := b.offs[i]
	c := b.chunks[o>>32]
	s := int(uint32(o)) + int(b.klens[i])
	return c[s : s+int(b.vlens[i])]
}

// ---------------------------------------------------------------- parsing --

// rowSink receives parsed rows. It records the earliest limit violation and
// stops storing rows after it (rows after the first violation can never be
// reported, but later parse errors still take precedence).
type rowSink struct {
	b        *Builder
	limitErr *LimitError
}

func (s *rowSink) add(key, value []byte, line int) {
	if s.limitErr != nil {
		return
	}
	if len(key) > MaxKeyLen {
		s.limitErr = &LimitError{Line: line, Field: "key", Len: len(key), Max: MaxKeyLen}
		return
	}
	if len(value) > MaxValueLen {
		s.limitErr = &LimitError{Line: line, Field: "value", Len: len(value), Max: MaxValueLen}
		return
	}
	s.b.appendRow(key, value, line)
}

func isFormatSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

func (b *Builder) parse(ctx context.Context, src io.Reader, format Format) error {
	from := len(b.offs)
	br := bufio.NewReaderSize(src, 1<<20)
	// Detect the first non-whitespace byte without consuming it from the
	// logical stream (leading whitespace is significant to the CSV parser).
	var prefix []byte
	first := byte(0)
	empty := true
	for {
		c, err := br.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if !isFormatSpace(c) {
			first = c
			empty = false
			_ = br.UnreadByte()
			break
		}
		prefix = append(prefix, c)
	}
	if empty {
		return nil // empty data is valid (§1.5)
	}
	var r io.Reader = br
	if len(prefix) > 0 {
		r = io.MultiReader(bytes.NewReader(prefix), br)
	}
	if format == FormatAuto {
		if first == '{' {
			format = FormatNDJSON
		} else {
			format = FormatCSV
		}
	}
	sink := &rowSink{b: b}
	var err error
	switch format {
	case FormatCSV:
		err = parseCSV(ctx, r, sink)
	case FormatNDJSON:
		if br, ok := r.(*bufio.Reader); ok {
			err = parseNDJSON(ctx, br, sink)
		} else {
			err = parseNDJSON(ctx, bufio.NewReaderSize(r, 1<<20), sink)
		}
	default:
		return fmt.Errorf("attos: unknown format %d", format)
	}
	if err != nil {
		return err
	}
	if sink.limitErr != nil {
		// Rows before the limit violation may still contain a duplicate that
		// precedes it; limitOrDuplicate picks the earlier of the two.
		return b.limitOrDuplicate(sink.limitErr, from)
	}
	return nil
}

// limitOrDuplicate returns the earlier of the limit error and the first
// duplicate among the rows stored so far (all of which precede the limit row).
func (b *Builder) limitOrDuplicate(le *LimitError, from int) error {
	if d := b.firstDuplicate(from); d != nil && d.Line < le.Line {
		return d
	}
	return le
}

const ctxCheckEvery = 1 << 16

func parseCSV(ctx context.Context, r io.Reader, sink *rowSink) error {
	cr := csv.NewReader(r)
	cr.ReuseRecord = true
	header, err := cr.Read()
	if err != nil {
		if err == io.EOF {
			return nil
		}
		return &MalformedRowError{Line: 1, Reason: err.Error()}
	}
	cr.FieldsPerRecord = len(header)
	valIdx := -1
	type dim struct {
		name string
		idx  int
	}
	var dims []dim
	for i, h := range header {
		if h == valueColumn {
			if valIdx != -1 {
				return &MalformedRowError{Line: 1, Reason: "duplicate attos_value column"}
			}
			valIdx = i
		} else {
			dims = append(dims, dim{strings.Clone(h), i})
		}
	}
	if valIdx == -1 {
		return ErrMissingValueColumn
	}
	if len(dims) == 0 {
		return ErrNoDimensions
	}
	sort.SliceStable(dims, func(i, j int) bool { return dims[i].name < dims[j].name })

	key := make([]byte, 0, 256)
	for n := 0; ; n++ {
		if n%ctxCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("attos: build cancelled: %w", err)
			}
		}
		rec, err := cr.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			line := 0
			var pe *csv.ParseError
			if errors.As(err, &pe) {
				line = pe.StartLine
			}
			return &MalformedRowError{Line: line, Reason: err.Error()}
		}
		line, _ := cr.FieldPos(0)
		key = key[:0]
		for i, d := range dims {
			v := rec[d.idx]
			if strings.IndexByte(v, 0) >= 0 {
				return &MalformedRowError{Line: line, Reason: "NUL byte in dimension value"}
			}
			if i > 0 {
				key = append(key, 0)
			}
			key = append(key, v...)
		}
		v := rec[valIdx]
		sink.add(key, unsafe.Slice(unsafe.StringData(v), len(v)), line)
	}
}

func parseNDJSON(ctx context.Context, br *bufio.Reader, sink *rowSink) error {
	var dims []string
	legacy := false
	lineNo := 0
	var long []byte
	key := make([]byte, 0, 256)
	for {
		if lineNo%ctxCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("attos: build cancelled: %w", err)
			}
		}
		line, rerr := br.ReadSlice('\n')
		if rerr == bufio.ErrBufferFull {
			long = append(long[:0], line...)
			for rerr == bufio.ErrBufferFull {
				line, rerr = br.ReadSlice('\n')
				long = append(long, line...)
			}
			line = long
		}
		if rerr != nil && rerr != io.EOF {
			return rerr
		}
		if len(line) == 0 && rerr != nil {
			return nil
		}
		lineNo++
		t := bytes.TrimSpace(line)
		if len(t) == 0 {
			if rerr != nil {
				return nil
			}
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(t, &obj); err != nil {
			return &MalformedRowError{Line: lineNo, Reason: err.Error()}
		}
		if dims == nil {
			if _, hasVal := obj[valueColumn]; !hasVal {
				_, hk := obj["key"]
				_, hv := obj["value"]
				if hk && hv && len(obj) == 2 {
					legacy = true
				} else {
					return ErrMissingValueColumn
				}
			}
			for k := range obj {
				if legacy && k == "key" || !legacy && k != valueColumn {
					dims = append(dims, k)
				}
			}
			if len(dims) == 0 {
				return ErrNoDimensions
			}
			sort.Strings(dims)
		}
		if len(obj) != len(dims)+1 {
			return &MalformedRowError{Line: lineNo, Reason: "field set differs from first line"}
		}
		valField := valueColumn
		if legacy {
			valField = "value"
		}
		key = key[:0]
		for i, d := range dims {
			raw, ok := obj[d]
			if !ok {
				return &MalformedRowError{Line: lineNo, Reason: "missing field " + d}
			}
			s, err := jsonScalar(raw)
			if err != nil {
				return &MalformedRowError{Line: lineNo, Reason: d + ": " + err.Error()}
			}
			if bytes.IndexByte(s, 0) >= 0 {
				return &MalformedRowError{Line: lineNo, Reason: "NUL byte in dimension value"}
			}
			if i > 0 {
				key = append(key, 0)
			}
			key = append(key, s...)
		}
		raw, ok := obj[valField]
		if !ok {
			return &MalformedRowError{Line: lineNo, Reason: "missing field " + valField}
		}
		v, err := jsonScalar(raw)
		if err != nil {
			return &MalformedRowError{Line: lineNo, Reason: valField + ": " + err.Error()}
		}
		sink.add(key, v, lineNo)
		if rerr != nil {
			return nil
		}
	}
}

// jsonScalar returns the decoded text of a JSON string, or the literal token
// text of a JSON number. Anything else is an error (§1.5).
func jsonScalar(raw json.RawMessage) ([]byte, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, errors.New("empty")
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []byte(s), nil
	}
	if !(raw[0] == '-' || (raw[0] >= '0' && raw[0] <= '9')) {
		return nil, errors.New("value must be a JSON string or number")
	}
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return nil, errors.New("value must be a JSON string or number")
	}
	return raw, nil
}

// ------------------------------------------------------ duplicate detection --

func (b *Builder) ensureHashes() {
	for i := len(b.hashes); i < len(b.offs); i++ {
		b.hashes = append(b.hashes, xxhash.Sum64(b.rowKey(i)))
	}
}

// firstDuplicate returns the DuplicateKeyError for the lowest-indexed row
// (>= from) whose key equals some earlier row's key, or nil.
func (b *Builder) firstDuplicate(from int) *DuplicateKeyError {
	n := len(b.offs)
	if n-from <= 0 || n < 2 {
		return nil
	}
	b.ensureHashes()
	idx := make([]uint32, n)
	for i := range idx {
		idx[i] = uint32(i)
	}
	slices.SortFunc(idx, func(x, y uint32) int {
		hx, hy := b.hashes[x], b.hashes[y]
		if hx != hy {
			if hx < hy {
				return -1
			}
			return 1
		}
		return int(x) - int(y)
	})
	best := -1
	bestFirst := -1
	for i := 0; i < n; {
		j := i + 1
		for j < n && b.hashes[idx[j]] == b.hashes[idx[i]] {
			j++
		}
		if j-i > 1 {
			// Rows idx[i:j] share a hash (ascending row order). Find, for each
			// row, the earliest earlier row with an equal key.
			for p := i + 1; p < j; p++ {
				r := int(idx[p])
				if r < from || (best >= 0 && r >= best) {
					continue
				}
				kr := b.rowKey(r)
				for q := i; q < p; q++ {
					if bytes.Equal(b.rowKey(int(idx[q])), kr) {
						best, bestFirst = r, int(idx[q])
						break
					}
				}
			}
		}
		i = j
	}
	if best < 0 {
		return nil
	}
	return &DuplicateKeyError{FirstLine: int(b.lines[bestFirst]), Line: int(b.lines[best])}
}

// -------------------------------------------------------------- placement --

func isPrime(n uint64) bool {
	if n < 2 {
		return false
	}
	if n < 4 {
		return true
	}
	if n%2 == 0 || n%3 == 0 {
		return false
	}
	for i := uint64(5); i*i <= n; i += 6 {
		if n%i == 0 || n%(i+2) == 0 {
			return false
		}
	}
	return true
}

// tableSizes returns (S, B) for n rows (§1.3). Integer arithmetic only.
func tableSizes(n uint64) (uint64, uint64) {
	s := n * 12 / 10
	if s < 3 {
		s = 3
	}
	for !isPrime(s) {
		s++
	}
	bk := s * 6 / 5
	if bk < 1 {
		bk = 1
	}
	return s, bk
}

type placement struct {
	S, B    uint64
	disp    []uint32
	slotRow []uint32 // row index per slot; emptySlot if unoccupied
	maxD    uint32
}

const emptySlot = math.MaxUint32

func (b *Builder) place(ctx context.Context) (*placement, error) {
	n := uint64(len(b.offs))
	if n >= emptySlot {
		return nil, fmt.Errorf("%w: too many rows (%d)", ErrBlobTooLarge, n)
	}
	b.ensureHashes()
	S, B := tableSizes(n)
	p := &placement{S: S, B: B, disp: make([]uint32, B), slotRow: make([]uint32, S)}
	for i := range p.slotRow {
		p.slotRow[i] = emptySlot
	}
	if n == 0 {
		return p, nil
	}

	// Group rows by bucket (CSR layout).
	start := make([]uint32, B+1)
	for _, h := range b.hashes {
		start[uint64(uint32(h>>32))%B+1]++
	}
	maxSize := uint32(0)
	for i := uint64(1); i <= B; i++ {
		if start[i] > maxSize {
			maxSize = start[i]
		}
		start[i] += start[i-1]
	}
	members := make([]uint32, n)
	fill := make([]uint32, B)
	copy(fill, start[:B])
	for r, h := range b.hashes {
		bk := uint64(uint32(h>>32)) % B
		members[fill[bk]] = uint32(r)
		fill[bk]++
	}
	fill = nil

	// Order non-empty buckets by (size desc, index asc) with a counting sort.
	bySize := make([]uint32, maxSize+2)
	for bk := uint64(0); bk < B; bk++ {
		if sz := start[bk+1] - start[bk]; sz > 0 {
			bySize[maxSize-sz+1]++
		}
	}
	for i := 1; i < len(bySize); i++ {
		bySize[i] += bySize[i-1]
	}
	order := make([]uint32, bySize[len(bySize)-1])
	for bk := uint64(0); bk < B; bk++ {
		if sz := start[bk+1] - start[bk]; sz > 0 {
			pos := maxSize - sz
			order[bySize[pos]] = uint32(bk)
			bySize[pos]++
		}
	}

	occupied := make([]uint64, (S+63)/64)
	isOcc := func(s uint64) bool { return occupied[s>>6]&(1<<(s&63)) != 0 }
	los := make([]uint64, 0, maxSize)
	slots := make([]uint64, 0, maxSize)

	for oi, bk := range order {
		if oi%ctxCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("attos: build cancelled: %w", err)
			}
		}
		mem := members[start[bk]:start[uint64(bk)+1]]
		los = los[:0]
		for _, r := range mem {
			lo := uint64(uint32(b.hashes[r]))
			for _, o := range los {
				if o == lo {
					return nil, fmt.Errorf("%w: bucket %d has keys with equal low hash", ErrUnplaceable, bk)
				}
			}
			los = append(los, lo)
		}
		placed := false
		for d := uint64(0); d <= math.MaxUint32; d++ {
			slots = slots[:0]
			ok := true
			for _, lo := range los {
				s := (lo ^ d) % S
				if isOcc(s) {
					ok = false
					break
				}
				for _, o := range slots {
					if o == s {
						ok = false
						break
					}
				}
				if !ok {
					break
				}
				slots = append(slots, s)
			}
			if ok {
				for i, s := range slots {
					occupied[s>>6] |= 1 << (s & 63)
					p.slotRow[s] = mem[i]
				}
				p.disp[bk] = uint32(d)
				if uint32(d) > p.maxD {
					p.maxD = uint32(d)
				}
				placed = true
				break
			}
			if d&0xFFFFF == 0xFFFFF {
				if err := ctx.Err(); err != nil {
					return nil, fmt.Errorf("attos: build cancelled: %w", err)
				}
			}
		}
		if !placed {
			return nil, fmt.Errorf("%w: displacement exhausted for bucket %d", ErrUnplaceable, bk)
		}
	}
	return p, nil
}

// ---------------------------------------------------------------- output ----

// CompileTo writes the v1 blob for all accumulated rows to w. Nothing is
// written if duplicate detection or placement fails.
func (b *Builder) CompileTo(w io.Writer) (BuildStats, error) {
	return b.CompileToContext(context.Background(), w)
}

// CompileToContext is CompileTo with cancellation (checked every 64k rows /
// buckets and during placement). On cancellation nothing has been written.
func (b *Builder) CompileToContext(ctx context.Context, w io.Writer) (BuildStats, error) {
	p, err := b.prepare(ctx)
	if err != nil {
		return BuildStats{}, err
	}
	return b.write(p, w)
}

// prepare runs duplicate detection (if rows were added via Add since the last
// check), placement and the data-block size check.
func (b *Builder) prepare(ctx context.Context) (*placement, error) {
	if b.checked < len(b.offs) {
		if d := b.firstDuplicate(b.checked); d != nil {
			return nil, d
		}
		b.checked = len(b.offs)
	}
	p, err := b.place(ctx)
	if err != nil {
		return nil, err
	}
	if b.dataBytes > maxDataBlock {
		return nil, fmt.Errorf("%w: data block %d bytes > %d", ErrBlobTooLarge, b.dataBytes, uint64(maxDataBlock))
	}
	return p, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func (b *Builder) write(p *placement, dst io.Writer) (BuildStats, error) {
	cw := &countingWriter{w: dst}
	bw := bufio.NewWriterSize(cw, 4<<20)
	crc := crc32.NewIEEE()
	mw := io.MultiWriter(bw, crc)
	be := binary.BigEndian

	flag := byte(0x01)
	if p.maxD > 0xFFFF {
		flag = 0x02
	}
	var hdr [33]byte
	copy(hdr[0:4], magicATTO)
	be.PutUint32(hdr[4:8], 1)
	hdr[8] = flag
	be.PutUint64(hdr[9:17], p.S)
	be.PutUint64(hdr[17:25], p.B)
	be.PutUint64(hdr[25:33], 0)
	if _, err := mw.Write(hdr[:]); err != nil {
		return BuildStats{}, err
	}

	buf := make([]byte, 0, 64<<10)
	flush := func(force bool) error {
		if len(buf) == 0 || (!force && len(buf) < cap(buf)-16) {
			return nil
		}
		_, err := mw.Write(buf)
		buf = buf[:0]
		return err
	}
	for _, d := range p.disp {
		if flag == 0x01 {
			buf = be.AppendUint16(buf, uint16(d))
		} else {
			buf = be.AppendUint32(buf, d)
		}
		if err := flush(false); err != nil {
			return BuildStats{}, err
		}
	}
	var off uint64
	for _, r := range p.slotRow {
		var word uint64
		if r != emptySlot {
			ln := uint64(2 + uint64(b.klens[r]) + uint64(b.vlens[r]))
			word = off<<32 | ln
			off += ln
		}
		buf = be.AppendUint64(buf, word)
		if err := flush(false); err != nil {
			return BuildStats{}, err
		}
	}
	if err := flush(true); err != nil {
		return BuildStats{}, err
	}
	for _, r := range p.slotRow {
		if r == emptySlot {
			continue
		}
		var kl [2]byte
		be.PutUint16(kl[:], b.klens[r])
		if _, err := mw.Write(kl[:]); err != nil {
			return BuildStats{}, err
		}
		if _, err := mw.Write(b.rowKey(int(r))); err != nil {
			return BuildStats{}, err
		}
		if _, err := mw.Write(b.rowValue(int(r))); err != nil {
			return BuildStats{}, err
		}
	}
	sum := crc.Sum32()
	var tr [4]byte
	be.PutUint32(tr[:], sum)
	if _, err := bw.Write(tr[:]); err != nil {
		return BuildStats{}, err
	}
	if err := bw.Flush(); err != nil {
		return BuildStats{}, err
	}
	return BuildStats{
		Rows:            uint64(len(b.offs)),
		Slots:           p.S,
		Buckets:         p.B,
		MaxDisplacement: p.maxD,
		Bytes:           cw.n,
		CRC32:           sum,
	}, nil
}

// Compile writes the blob to outPath atomically: it writes a temporary file in
// the same directory, fsyncs it and renames it over outPath. On error outPath
// is untouched and the temporary file is removed.
// CompileV1 writes the legacy hash-and-displace format (version 1). Kept for compatibility and tests;
// new datasets should use Compile.
func (b *Builder) CompileV1(outPath string) error {
	_, err := b.CompileFile(context.Background(), outPath)
	return err
}

// CompileFile is Compile with cancellation and statistics.
func (b *Builder) CompileFile(ctx context.Context, outPath string) (BuildStats, error) {
	p, err := b.prepare(ctx)
	if err != nil {
		return BuildStats{}, err
	}
	dir := filepath.Dir(outPath)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(outPath)+".tmp-*")
	if err != nil {
		return BuildStats{}, err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	st, err := b.write(p, tmp)
	if err != nil {
		return BuildStats{}, err
	}
	if err := tmp.Sync(); err != nil {
		return BuildStats{}, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		return BuildStats{}, err
	}
	if err := tmp.Close(); err != nil {
		return BuildStats{}, err
	}
	if err := os.Rename(tmpName, outPath); err != nil {
		return BuildStats{}, err
	}
	ok = true
	return st, nil
}

// Compile reads src fully, writes one complete v1 blob to dst and returns its
// statistics. It mirrors attos-cloud/engine/compiler.Compile (CONTRACTS.md §3)
// and produces byte-identical output. Nothing is written to dst if parsing,
// duplicate detection or placement fails.
func Compile(ctx context.Context, src io.Reader, format Format, dst io.Writer) (BuildStats, error) {
	b := NewBuilder()
	if err := b.BuildFrom(ctx, src, format); err != nil {
		return BuildStats{}, err
	}
	return b.CompileToContext(ctx, dst)
}

// ------------------------------------------------------- legacy helpers ----

// MmapFile maps path read-only. Deprecated: retained for API compatibility.
func MmapFile(path string) ([]byte, *os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if fi.Size() == 0 {
		f.Close()
		return nil, nil, fmt.Errorf("file is empty")
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(fi.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return data, f, nil
}

// MunmapFile releases a mapping returned by MmapFile. Deprecated.
func MunmapFile(data []byte, f *os.File) {
	if len(data) > 0 {
		_ = syscall.Munmap(data)
	}
	_ = f.Close()
}
