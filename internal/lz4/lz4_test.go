package lz4

import (
	"bytes"
	"math/rand"
	"testing"
)

func roundTrip(t *testing.T, src []byte) []byte {
	t.Helper()
	c := Compress(nil, src)
	dst := make([]byte, len(src))
	if err := Decompress(dst, c); err != nil {
		t.Fatalf("len %d: %v", len(src), err)
	}
	if !bytes.Equal(dst, src) {
		t.Fatalf("len %d: mismatch", len(src))
	}
	return c
}

func TestRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for n := 0; n < 300; n++ {
		b := make([]byte, n)
		r.Read(b)
		roundTrip(t, b)
		for i := range b {
			b[i] = byte(i % 7)
		}
		roundTrip(t, b)
	}
	text := bytes.Repeat([]byte(`{"id":12345,"name":"alpha beta","plan":"pro"}`), 400)
	if c := roundTrip(t, text); len(c) > len(text)/10 {
		t.Fatalf("poor compression: %d -> %d", len(text), len(c))
	}
	big := make([]byte, 1<<20)
	for i := range big {
		big[i] = byte(r.Intn(4))
	}
	roundTrip(t, big)
}

func TestDecompressHostile(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	good := Compress(nil, bytes.Repeat([]byte("hello world "), 100))
	for i := 0; i < 20000; i++ {
		b := append([]byte(nil), good...)
		for j := 0; j < 1+r.Intn(4); j++ {
			b[r.Intn(len(b))] = byte(r.Intn(256))
		}
		b = b[:r.Intn(len(b)+1)]
		dst := make([]byte, 1200)
		_ = Decompress(dst, b) // must not panic
	}
}

func TestSlackFastPathMatchesPlain(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	for i := 0; i < 3000; i++ {
		n := 1 + r.Intn(3000)
		src := make([]byte, n)
		for j := range src {
			src[j] = byte(r.Intn(1 + i%9))
		}
		c := Compress(nil, src)
		a := make([]byte, n)
		b := make([]byte, n, n+Slack)
		if err := Decompress(a, c); err != nil {
			t.Fatal(err)
		}
		if err := Decompress(b, c); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, src) || !bytes.Equal(b, src) {
			t.Fatalf("mismatch at iteration %d", i)
		}
	}
}
