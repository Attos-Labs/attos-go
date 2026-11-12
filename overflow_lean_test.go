package attos

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The lean-key variant of the shared overflow vectors (written by the cloud compiler): a lean main (8-byte
// fingerprints) plus an overflow whose entries carry full keys. Applying the tail patch to the base must give
// lean_v2_expected.attos byte for byte, and every SDK must read it as the new dataset.
func TestLeanOverflowVectors(t *testing.T) {
	read := func(name, sha string) []byte {
		b, err := os.ReadFile(filepath.Join("testdata", "overflow", name))
		if err != nil {
			t.Fatal(err)
		}
		if s := sha256.Sum256(b); fmt.Sprintf("%x", s) != sha {
			t.Fatalf("%s changed: %x", name, s)
		}
		return b
	}
	base := read("lean_v1_base.attos", "b5215f3223cd00cd9ae5aaae5c7a28729e8b75e42f7bccf8c39a182797df5e06")
	patch := read("lean_v1_to_v2_tail.patch", "51a67c3f41dca28659c7688172b246a315b5e76c865af677cd69f69193e5638a")
	exp := read("lean_v2_expected.attos", "5377ebfe08c19040f1d72441fe359715b8059fbbec7c49b2aa3514ba7bafcdb6")
	if got, err := apply(base, patch); err != nil || !bytes.Equal(got, exp) {
		t.Fatalf("tail patch does not rebuild lean_v2_expected: %v", err)
	}
	if exp[8] != 0x06 {
		t.Fatalf("flags %#x, want 0x06 (lean + overflow)", exp[8])
	}
	if mb := openBlob(t, base); mb.Len() != 1000 {
		t.Fatalf("base Len %d", mb.Len())
	}
	m := openBlob(t, exp)
	if m.Len() != 1048 {
		t.Fatalf("Len %d, want 1048", m.Len())
	}
	get := func(k string) (string, error) { v, err := m.GetCopy([]byte(k), nil); return string(v), err }
	for i := 0; i < 50; i++ {
		if v, err := get(fmt.Sprintf("ins_%d", i)); err != nil || v != fmt.Sprintf("inserted_%d", i) {
			t.Fatalf("ins_%d = %q, %v", i, v, err)
		}
	}
	changed := map[int]bool{10: true, 20: true, 30: true, 40: true, 50: true, 100: true, 200: true}
	for _, i := range []int{10, 20, 30, 40, 50} {
		if v, err := get(fmt.Sprintf("key_%d", i)); err != nil || v != fmt.Sprintf("overwritten_%d", i) {
			t.Fatalf("key_%d = %q, %v", i, v, err)
		}
	}
	for _, k := range []string{"key_100", "key_200", "nope"} {
		if _, err := get(k); !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("%s must be absent: %v", k, err)
		}
	}
	for i := 0; i < 1000; i++ {
		if changed[i] {
			continue
		}
		if v, err := get(fmt.Sprintf("key_%d", i)); err != nil || v != fmt.Sprintf("val_%d", i) {
			t.Fatalf("key_%d = %q, %v", i, v, err)
		}
	}
}
