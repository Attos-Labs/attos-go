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
	keyHash := xxhash.Sum64String(keyStr)
	low := uint32(keyHash)

	totalKeys := uint64(1)
	numBuckets := uint64(1)

	buf := make([]byte, 1024)
	copy(buf[0:4], "ATTO")
	binary.BigEndian.PutUint32(buf[4:8], 1) // version
	buf[8] = 0x01 // 16-bit displacement
	binary.BigEndian.PutUint64(buf[9:17], totalKeys)
	binary.BigEndian.PutUint64(buf[17:25], numBuckets)
	binary.BigEndian.PutUint64(buf[25:33], 0) // global seed

	// Routing Table at 33
	d := uint16(0)
	binary.BigEndian.PutUint16(buf[33:35], d)

	finalIndex := (uint64(low) ^ uint64(d)) % totalKeys

	// Values array at 35
	payloadStr := "value_123"
	payloadOffset := uint32(0)
	payloadLen := uint32(len(payloadStr))
	
	packed := (uint64(payloadOffset) << 32) | uint64(payloadLen)
	valueOff := 35 + int(finalIndex*8)
	binary.BigEndian.PutUint64(buf[valueOff:valueOff+8], packed)

	// Blob at 35 + 8 = 43
	blobOff := 35 + int(totalKeys*8)
	copy(buf[blobOff:blobOff+int(payloadLen)], payloadStr)

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
