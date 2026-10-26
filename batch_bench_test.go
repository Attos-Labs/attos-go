package attos

import (
	"math/rand"
	"os"
	"testing"
)

// BenchmarkGetMany compares Get with GetMany on a real file:
//
//	ATTOS_BENCH_FILE=dataset.attos go test -run xxx -bench 'Single|Batch' .
func benchKeys(b *testing.B) (*Map, [][]byte) {
	path := os.Getenv("ATTOS_BENCH_FILE")
	if path == "" {
		b.Skip("set ATTOS_BENCH_FILE to a .attos file")
	}
	m, err := Open(path)
	if err != nil {
		b.Fatal(err)
	}
	var keys [][]byte
	m.Scan(func(k, _ []byte) bool {
		keys = append(keys, append([]byte(nil), k...))
		return len(keys) < 2_000_000
	})
	rand.New(rand.NewSource(1)).Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	return m, keys
}

func BenchmarkSingleGet(b *testing.B) {
	m, keys := benchKeys(b)
	defer m.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = m.Get(keys[i%len(keys)])
	}
}

func BenchmarkBatchGet256(b *testing.B) {
	m, keys := benchKeys(b)
	defer m.Close()
	out := make([][]byte, 256)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i += 256 {
		off := (i % (len(keys) - 256))
		_, _ = m.GetMany(keys[off:off+256], out)
	}
}
