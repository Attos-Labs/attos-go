package attos

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/Attos-Labs/attos-go/internal/hash"
)

func createMockAttosFile(t testing.TB) string {
	t.Helper()

	keyStr := "user_123"
	keyHash := hash.String(keyStr)
	sig := uint32(keyHash >> 32)
	idx := hash.Block(keyHash, 0, 0) % 64

	buf := make([]byte, 1024)
	copy(buf[0:4], "MPHB")
	binary.BigEndian.PutUint32(buf[4:8], 0) // flags
	binary.BigEndian.PutUint64(buf[24:32], 64) // nkeys = 64
	binary.BigEndian.PutUint64(buf[32:40], 64) // offtbl = 64

	// We fill 65 entries (64 keys + 1 dummy)
	dummyEntry := (uint64(sig) << 32) | uint64(1009) // Also give dummy sig to fail expectedly if something goes wrong
	for i := 0; i < 65; i++ {
		binary.LittleEndian.PutUint64(buf[64+i*8:72+i*8], dummyEntry)
	}

	// Place our real entry at idx
	valOffset := uint32(1000)
	entry1 := (uint64(sig) << 32) | uint64(valOffset)
	binary.LittleEndian.PutUint64(buf[64+int(idx)*8:72+int(idx)*8], entry1)

	// Map Index at 584
	buf[584] = 1
	binary.LittleEndian.PutUint32(buf[588:592], 1)
	binary.LittleEndian.PutUint64(buf[600:608], 1)
	binary.LittleEndian.PutUint64(buf[608:616], ^uint64(0))

	// Value at 1000
	copy(buf[1000:1009], "value_123")

	dir := t.TempDir()
	path := filepath.Join(dir, "mock.nh")
	if err := os.WriteFile(path, buf, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReader_Get(t *testing.T) {
	path := createMockAttosFile(t)
	
	// Test standard Open
	db, err := Open(path)
	if err != nil {
		t.Fatalf("failed to open mock db: %v", err)
	}
	defer db.Close()

	// Test successful lookup (byte slice)
	valBytes, err := db.Get([]byte("user_123"))
	if err != nil {
		t.Errorf("Get(user_123) []byte failed: %v", err)
	} else if string(valBytes) != "value_123" {
		t.Errorf("expected 'value_123', got %q", string(valBytes))
	}

	// Test successful lookup (string)
	valStr, err := db.GetString("user_123")
	if err != nil {
		t.Errorf("GetString(user_123) failed: %v", err)
	} else if valStr != "value_123" {
		t.Errorf("expected 'value_123', got %q", valStr)
	}

	// Test missing key
	_, err = db.GetString("missing_key")
	if err == nil {
		t.Error("expected error for missing key")
	}
}

func BenchmarkReader_Get(b *testing.B) {
	pathMock := createMockAttosFile(b)
	db, err := Open(pathMock)
	if err != nil {
		b.Fatalf("failed to open mock db: %v", err)
	}
	defer db.Close()

	numKeys := 1024
	keys := make([]string, numKeys)
	for i := 0; i < numKeys; i++ {
		keys[i] = "user_123" // Test best-case path caching and latency
	}
	mask := numKeys - 1

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		k := keys[i&mask]
		_, _ = db.GetString(k)
	}
}
