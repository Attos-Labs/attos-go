package attos

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

// Every overflow record is validated when the file is opened (OVERFLOW_BLOCK.md, "Validation on open/sync": valLen
// bounds), not when a lookup happens to meet it: a mapping that opens never holds a malformed record. Rust, Java,
// Node and Python behave the same way.
func TestOverflowRecordsAreValidatedAtOpen(t *testing.T) {
	base := buildMode(t, 100, CompileOptions{})
	blob := withOverflow(t, base, []ovItem{
		{key: []byte("a"), val: []byte("1")}, {key: []byte("b"), tomb: true}, {key: []byte("c"), val: []byte("333")},
	})
	k := int(binary.BigEndian.Uint64(blob[len(blob)-24:]))
	baseEnd := int(binary.BigEndian.Uint64(blob[len(blob)-16:]))
	dataOff := baseEnd + k*8 + (k+1)*8
	dir := t.TempDir()
	open := func(c []byte) (*Map, error) {
		binary.BigEndian.PutUint32(c[len(c)-4:], crc32.ChecksumIEEE(c[:len(c)-4]))
		p := filepath.Join(dir, "r.attos")
		if err := os.WriteFile(p, c, 0o600); err != nil {
			return nil, err
		}
		return OpenWithConfig(p, OpenConfig{ValidateChecksum: true})
	}
	// the untouched file opens
	m, err := open(append([]byte(nil), blob...))
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	// Targeted malformations of the first record (u16 keyLen | u32 valLen | key | value), CRC fixed up so only the
	// record is wrong: all must be refused at open.
	cases := map[string]func(c []byte){
		"keyLen beyond the record": func(c []byte) { c[dataOff], c[dataOff+1] = 0xff, 0xff },
		"valLen beyond the record": func(c []byte) { c[dataOff+2], c[dataOff+3], c[dataOff+4], c[dataOff+5] = 0, 0x10, 0, 0 },
		"valLen one too long":      func(c []byte) { c[dataOff+5]++ },
		"keyLen one too short":     func(c []byte) { c[dataOff+1]-- },
		"value record turned into a tombstone": func(c []byte) {
			// the first record in hash order may itself be a tombstone: pick the first record that carries a value
			offOff := baseEnd + k*8
			for e := 0; e < k; e++ {
				s := dataOff + int(binary.LittleEndian.Uint64(c[offOff+e*8:]))
				if binary.BigEndian.Uint32(c[s+2:]) != 0xFFFFFFFF {
					c[s+2], c[s+3], c[s+4], c[s+5] = 0xff, 0xff, 0xff, 0xff
					return
				}
			}
			t.Fatal("no value record in the test file")
		},
	}
	for name, mut := range cases {
		c := append([]byte(nil), blob...)
		mut(c)
		if m, err := open(c); err == nil {
			m.Close()
			t.Errorf("%s: opened", name)
		}
	}
	// And a sweep: whatever single byte of the data region is flipped, a file that opens has only well-formed records.
	for i := dataOff; i < len(blob)-24; i++ {
		for _, x := range []byte{0x01, 0x80, 0xff} {
			c := append([]byte(nil), blob...)
			c[i] ^= x
			m, err := open(c)
			if err != nil {
				continue
			}
			mp := m.cur.Load()
			for e := 0; e < mp.ovK; e++ {
				if _, _, _, ok := mp.ovEntry(e); !ok {
					t.Fatalf("byte %d ^ %#x: opened with a malformed record %d", i, x, e)
				}
			}
			m.Close()
		}
	}
}

// A v2 patch consists of one TRUNCATE_AND_APPEND op and END (OVERFLOW_SHIPPED.md); the v1 copy/add ops are not part of
// it and are refused, even when the rest of the patch is valid. Rust, Java, Node and Python behave the same way.
func TestTailPatchRefusesVersion1Ops(t *testing.T) {
	base := buildMode(t, 100, CompileOptions{})
	nw := withOverflow(t, base, []ovItem{{key: []byte("a"), val: []byte("1")}})
	good := makeTailPatch(base, nw)
	apply := func(p []byte) error {
		var out bytes.Buffer
		_, err := applyDelta(&out, bytes.NewReader(base), int64(len(base)), bytes.NewReader(p))
		return err
	}
	if err := apply(good); err != nil {
		t.Fatalf("the unmodified tail patch must apply: %v", err)
	}
	// insert a harmless ADD of zero bytes (0x02 0x00) before END and recompute the patch CRC
	insert := func(op []byte) []byte {
		body := append([]byte(nil), good[:len(good)-5]...) // drop END + CRC
		body = append(body, op...)
		body = append(body, 0) // END
		return binary.BigEndian.AppendUint32(body, crc32.ChecksumIEEE(body))
	}
	for name, op := range map[string][]byte{"ADD 0 bytes": {0x02, 0x00}, "ADD 1 byte": {0x02, 0x01, 'x'}, "COPY 0 bytes": {0x01, 0x00, 0x00}} {
		if err := apply(insert(op)); err == nil {
			t.Errorf("%s inside a v2 patch was accepted", name)
		}
	}
	// a v2 patch that starts with a v1 op
	hdr := append([]byte(nil), good[:60]...)
	body := append(hdr, 0x02, 0x00, 0x00)
	body = binary.BigEndian.AppendUint32(body, crc32.ChecksumIEEE(body))
	if err := apply(body); err == nil {
		t.Error("a v2 patch starting with ADD was accepted")
	}
}
