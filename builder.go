package attos

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/cespare/xxhash/v2"
)

var bufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, 1024)
		return &buf
	},
}

type ColumnMeta struct {
	Name  string
	Index int
}

type FastCSVParser struct {
	data      []byte
	numCols   int
	valColIdx int
	dimCols   []ColumnMeta
}

func NewFastCSVParser(data []byte) (*FastCSVParser, int, error) {
	headerEnd := bytes.IndexByte(data, '\n')
	if headerEnd == -1 {
		return nil, 0, fmt.Errorf("empty or malformed CSV")
	}

	headerLine := data[:headerEnd]
	if len(headerLine) > 0 && headerLine[len(headerLine)-1] == '\r' {
		headerLine = headerLine[:len(headerLine)-1]
	}

	var rawCols [][]byte
	start := 0
	for i := 0; i <= len(headerLine); i++ {
		if i == len(headerLine) || headerLine[i] == ',' {
			rawCols = append(rawCols, headerLine[start:i])
			start = i + 1
		}
	}

	valColIdx := -1
	var dims []ColumnMeta

	for idx, col := range rawCols {
		colStr := string(col)
		if colStr == "attos_value" {
			valColIdx = idx
		} else {
			dims = append(dims, ColumnMeta{Name: colStr, Index: idx})
		}
	}

	if valColIdx == -1 {
		return nil, 0, fmt.Errorf("missing required 'attos_value' column")
	}

	sort.Slice(dims, func(i, j int) bool {
		return dims[i].Name < dims[j].Name
	})

	return &FastCSVParser{
		data:      data,
		numCols:   len(rawCols),
		valColIdx: valColIdx,
		dimCols:   dims,
	}, headerEnd + 1, nil
}

func (p *FastCSVParser) ProcessRows(startOffset int, fn func(key []byte, val []byte) error) error {
	bufPtr := bufferPool.Get().(*[]byte)
	defer func() {
		*bufPtr = (*bufPtr)[:0]
		bufferPool.Put(bufPtr)
	}()

	data := p.data[startOffset:]
	colOffsets := make([][2]int, p.numCols)

	for len(data) > 0 {
		lineEnd := bytes.IndexByte(data, '\n')
		if lineEnd == -1 {
			lineEnd = len(data)
		}

		line := data[:lineEnd]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}

		if len(line) > 0 {
			colCount := 0
			cStart := 0
			for i := 0; i <= len(line); i++ {
				if i == len(line) || line[i] == ',' {
					if colCount < p.numCols {
						colOffsets[colCount] = [2]int{cStart, i}
						colCount++
					}
					cStart = i + 1
				}
			}

			if colCount == p.numCols {
				keyBuf := (*bufPtr)[:0]
				for idx, dim := range p.dimCols {
					off := colOffsets[dim.Index]
					if idx > 0 {
						keyBuf = append(keyBuf, 0x00)
					}
					keyBuf = append(keyBuf, line[off[0]:off[1]]...)
				}

				valOff := colOffsets[p.valColIdx]
				valSlice := line[valOff[0]:valOff[1]]

				if err := fn(keyBuf, valSlice); err != nil {
					return err
				}
			}
		}

		if lineEnd == len(data) {
			break
		}
		data = data[lineEnd+1:]
	}
	return nil
}

type FastJSONParser struct {
	data    []byte
	dimKeys [][]byte
	valKey  []byte
}

func NewFastJSONParser(data []byte) (*FastJSONParser, int, error) {
	firstLineEnd := bytes.IndexByte(data, '\n')
	if firstLineEnd == -1 {
		firstLineEnd = len(data)
	}

	line := data[:firstLineEnd]

	var keys [][]byte
	idx := 0
	for idx < len(line) {
		qIdx := bytes.IndexByte(line[idx:], '"')
		if qIdx == -1 {
			break
		}
		start := idx + qIdx + 1
		endIdx := bytes.IndexByte(line[start:], '"')
		if endIdx == -1 {
			break
		}
		key := line[start : start+endIdx]

		colonIdx := bytes.IndexByte(line[start+endIdx+1:], ':')
		if colonIdx != -1 && colonIdx < 5 {
			keys = append(keys, key)
			idx = start + endIdx + 1 + colonIdx + 1

			for idx < len(line) && line[idx] != ',' && line[idx] != '}' {
				if line[idx] == '"' {
					idx++
					for idx < len(line) && line[idx] != '"' {
						if line[idx] == '\\' {
							idx++
						}
						idx++
					}
				}
				idx++
			}
		} else {
			idx = start + endIdx + 1
		}
	}

	var dimKeys [][]byte
	hasVal := false
	for _, k := range keys {
		if string(k) == "attos_value" {
			hasVal = true
		} else {
			dimKeys = append(dimKeys, k)
		}
	}

	if !hasVal {
		return nil, 0, errors.New("missing attos_value field in first NDJSON row")
	}

	sort.Slice(dimKeys, func(i, j int) bool {
		return string(dimKeys[i]) < string(dimKeys[j])
	})

	return &FastJSONParser{
		data:    data,
		dimKeys: dimKeys,
		valKey:  []byte(`"attos_value":`),
	}, 0, nil
}

func (p *FastJSONParser) ProcessRows(startOffset int, fn func(key []byte, val []byte) error) error {
	bufPtr := bufferPool.Get().(*[]byte)
	defer func() {
		*bufPtr = (*bufPtr)[:0]
		bufferPool.Put(bufPtr)
	}()

	data := p.data[startOffset:]

	searchKeys := make([][]byte, len(p.dimKeys))
	for i, k := range p.dimKeys {
		searchKeys[i] = append(append([]byte(`"`), k...), []byte(`":`)...)
	}

	for len(data) > 0 {
		lineEnd := bytes.IndexByte(data, '\n')
		if lineEnd == -1 {
			lineEnd = len(data)
		}

		line := data[:lineEnd]

		if len(line) > 0 {
			keyBuf := (*bufPtr)[:0]
			validRow := true

			for idx, sKey := range searchKeys {
				kIdx := bytes.Index(line, sKey)
				if kIdx == -1 {
					validRow = false
					break
				}
				vStart := kIdx + len(sKey)

				for vStart < len(line) && (line[vStart] == ' ' || line[vStart] == '\t') {
					vStart++
				}

				vEnd := vStart
				if vStart < len(line) && line[vStart] == '"' {
					vStart++
					vEnd = vStart
					for vEnd < len(line) && line[vEnd] != '"' {
						if line[vEnd] == '\\' {
							vEnd++
						}
						vEnd++
					}
					vEnd++
					vEnd--
				} else {
					for vEnd < len(line) && line[vEnd] != ',' && line[vEnd] != '}' && line[vEnd] != ' ' {
						vEnd++
					}
				}

				if idx > 0 {
					keyBuf = append(keyBuf, 0x00)
				}
				keyBuf = append(keyBuf, line[vStart:vEnd]...)
			}

			vIdx := bytes.Index(line, p.valKey)
			if validRow && vIdx != -1 {
				vStart := vIdx + len(p.valKey)
				for vStart < len(line) && (line[vStart] == ' ' || line[vStart] == '\t') {
					vStart++
				}
				vEnd := vStart
				if vStart < len(line) && line[vStart] == '"' {
					vStart++
					vEnd = vStart
					for vEnd < len(line) && line[vEnd] != '"' {
						if line[vEnd] == '\\' {
							vEnd++
						}
						vEnd++
					}
					vEnd++
					vEnd--
				} else {
					for vEnd < len(line) && line[vEnd] != ',' && line[vEnd] != '}' && line[vEnd] != ' ' {
						vEnd++
					}
				}

				valSlice := line[vStart:vEnd]

				if err := fn(keyBuf, valSlice); err != nil {
					return err
				}
			}
		}

		if lineEnd == len(data) {
			break
		}
		data = data[lineEnd+1:]
	}
	return nil
}

type entry struct {
	key   []byte
	value []byte
}

type Builder struct {
	entries []entry
}

func NewBuilder() *Builder {
	return &Builder{}
}

func (b *Builder) BuildFromCSV(csvPath string) error {
	data, f, err := MmapFile(csvPath)
	if err != nil {
		return err
	}
	defer MunmapFile(data, f)

	parser, startOffset, err := NewFastCSVParser(data)
	if err != nil {
		return err
	}

	return parser.ProcessRows(startOffset, func(key []byte, val []byte) error {
		k := make([]byte, len(key))
		copy(k, key)
		v := make([]byte, len(val))
		copy(v, val)
		b.entries = append(b.entries, entry{key: k, value: v})
		return nil
	})
}

func (b *Builder) BuildFromNDJSON(jsonPath string) error {
	data, f, err := MmapFile(jsonPath)
	if err != nil {
		return err
	}
	defer MunmapFile(data, f)

	parser, startOffset, err := NewFastJSONParser(data)
	if err != nil {
		return err
	}

	return parser.ProcessRows(startOffset, func(key []byte, val []byte) error {
		k := make([]byte, len(key))
		copy(k, key)
		v := make([]byte, len(val))
		copy(v, val)
		b.entries = append(b.entries, entry{key: k, value: v})
		return nil
	})
}

func (b *Builder) Compile(outPath string) error {
	numEntries := len(b.entries)
	if numEntries == 0 {
		return errors.New("no entries to compile")
	}

	numKeys := uint64(numEntries) * 12 / 10
	if numKeys < 3 {
		numKeys = 3
	}
	for !isPrime(numKeys) {
		numKeys++
	}

	numBuckets := numKeys / 2
	if numBuckets == 0 {
		numBuckets = 1
	}

	type bucketData struct {
		keys []uint32
		idxs []int
	}
	buckets := make([]bucketData, numBuckets)

	for i, e := range b.entries {
		h := xxhash.Sum64(e.key)
		high := uint32(h >> 32)
		bIdx := uint64(high) % numBuckets
		buckets[bIdx].keys = append(buckets[bIdx].keys, uint32(h))
		buckets[bIdx].idxs = append(buckets[bIdx].idxs, i)
	}

	displacements := make([]uint32, numBuckets)
	finalMapping := make([]int32, numKeys)
	for i := range finalMapping {
		finalMapping[i] = -1
	}

	numWorkers := runtime.NumCPU()
	var maxDisp uint32
	var currentBucket uint64

	var wg sync.WaitGroup
	wg.Add(numWorkers)

	errCh := make(chan error, numWorkers)

	for w := 0; w < numWorkers; w++ {
		go func() {
			defer wg.Done()
			for {
				bIdx := atomic.AddUint64(&currentBucket, 1) - 1
				if bIdx >= uint64(len(buckets)) {
					return
				}

				bucket := buckets[bIdx]
				if len(bucket.keys) == 0 {
					continue
				}

				var seed uint32 = 0
			outer:
				for {
					if seed > 0xFFFFFFFE {
						errCh <- errors.New("displacement seed overflow")
						return
					}

					var tempSlots [256]int32
					tempCount := 0
					conflict := false
					for _, low := range bucket.keys {
						fIdx := int32((uint64(low) ^ uint64(seed)) % numKeys)
						for j := 0; j < tempCount; j++ {
							if tempSlots[j] == fIdx {
								conflict = true
								break
							}
						}
						if conflict {
							break
						}
						tempSlots[tempCount] = fIdx
						tempCount++
					}
					if conflict {
						seed++
						continue outer
					}

					for i := 0; i < tempCount; i++ {
						if !atomic.CompareAndSwapInt32(&finalMapping[tempSlots[i]], -1, -2) {
							for j := 0; j < i; j++ {
								atomic.StoreInt32(&finalMapping[tempSlots[j]], -1)
							}
							seed++
							continue outer
						}
					}

					for i := 0; i < tempCount; i++ {
						atomic.StoreInt32(&finalMapping[tempSlots[i]], int32(bucket.idxs[i]))
					}

					displacements[bIdx] = seed
					for {
						currMax := atomic.LoadUint32(&maxDisp)
						if seed <= currMax || atomic.CompareAndSwapUint32(&maxDisp, currMax, seed) {
							break
						}
					}
					break
				}
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}

	is32Bit := maxDisp > 65535
	flag := byte(0x01)
	if is32Bit {
		flag = 0x02
	}

	var blob bytes.Buffer
	valuesData := make([]uint64, numKeys)

	for i := uint64(0); i < numKeys; i++ {
		kIdx := finalMapping[i]
		if kIdx == -1 {
			continue
		}

		offset := blob.Len()
		k := b.entries[kIdx].key
		v := b.entries[kIdx].value
		kLen := uint16(len(k))

		binary.Write(&blob, binary.BigEndian, kLen)
		blob.Write(k)
		blob.Write(v)

		length := blob.Len() - offset
		packed := (uint64(offset) << 32) | uint64(length)
		valuesData[i] = packed
	}

	outF, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer outF.Close()

	bw := bufio.NewWriterSize(outF, 4*1024*1024)
	crcWriter := crc32.NewIEEE()
	mw := io.MultiWriter(bw, crcWriter)

	mw.Write([]byte("ATTO"))
	binary.Write(mw, binary.BigEndian, uint32(1))
	mw.Write([]byte{flag})
	binary.Write(mw, binary.BigEndian, uint64(numKeys))
	binary.Write(mw, binary.BigEndian, uint64(numBuckets))
	binary.Write(mw, binary.BigEndian, uint64(0))

	if is32Bit {
		for _, d := range displacements {
			binary.Write(mw, binary.BigEndian, d)
		}
	} else {
		for _, d := range displacements {
			binary.Write(mw, binary.BigEndian, uint16(d))
		}
	}

	for _, v := range valuesData {
		binary.Write(mw, binary.BigEndian, v)
	}

	mw.Write(blob.Bytes())

	binary.Write(bw, binary.BigEndian, crcWriter.Sum32())

	if err := bw.Flush(); err != nil {
		return err
	}

	return nil
}

func isPrime(n uint64) bool {
	if n <= 1 {
		return false
	}
	if n <= 3 {
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

func MunmapFile(data []byte, f *os.File) {
	if len(data) > 0 {
		_ = syscall.Munmap(data)
	}
	_ = f.Close()
}
