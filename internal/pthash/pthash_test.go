package pthash

import (
	"math/rand"
	"testing"
)

func TestMinimalPerfect(t *testing.T) {
	for _, n := range []int{1, 2, 3, 5, 17, 1000, 100000} {
		r := rand.New(rand.NewSource(int64(n)))
		seen := map[uint64]bool{}
		xs := make([]uint64, 0, n)
		for len(xs) < n {
			x := r.Uint64()
			if !seen[x] {
				seen[x] = true
				xs = append(xs, x)
			}
		}
		tb, err := Build(xs)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		used := make([]bool, n)
		for _, x := range xs {
			q := tb.Slot(x)
			if q >= uint64(n) || used[q] {
				t.Fatalf("n=%d: slot %d out of range or reused", n, q)
			}
			used[q] = true
		}
	}
}

func TestDuplicateHash(t *testing.T) {
	if _, err := Build([]uint64{5, 6, 5}); err != ErrDuplicateHash {
		t.Fatalf("want ErrDuplicateHash, got %v", err)
	}
}

// Golden values pin the hash functions; the Java and Rust SDKs assert the same constants.
func TestGoldenMix(t *testing.T) {
	if Mix(1) != 0x4179b061e0c0e0d0 || Mix(0xDEADBEEF) != 0x288344a3928b2668 || HP(0) != 0xc5209269cd34c3fc || Reduce(1<<63, 10) != 5 {
		t.Fatalf("hash constants changed: %#x %#x %#x %d", Mix(1), Mix(0xDEADBEEF), HP(0), Reduce(1<<63, 10))
	}
}
