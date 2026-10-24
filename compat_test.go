package attos

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// patchedGolden copies a golden file, applies mutate to the header, and rewrites the CRC trailer
// so the file looks like an intact file written by a newer SDK (fixCRC) or a damaged one (!fixCRC).
func patchedGolden(t *testing.T, name string, fixCRC bool, mutate func(b []byte)) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	mutate(b)
	if fixCRC {
		binary.BigEndian.PutUint32(b[len(b)-4:], crc32.ChecksumIEEE(b[:len(b)-4]))
	}
	p := filepath.Join(t.TempDir(), "patched.attos")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// An intact file that uses a feature newer than this SDK must fail closed with an error that
// names the feature and tells the operator to upgrade. A damaged file must stay "corrupted".
func TestUnsupportedFeaturesFailClosed(t *testing.T) {
	cases := []struct {
		name    string
		feature string
		mutate  func(b []byte)
	}{
		{"unknown flag bit", "flags", func(b []byte) { b[8] |= 0x04 }},
		{"offset width 6", "offset width 6", func(b []byte) { b[52] = 6 }},
		{"fingerprint width 16", "fingerprint width 16", func(b []byte) { b[8] |= 2; b[53] = 16 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := patchedGolden(t, "golden_v2_1000.attos", true, c.mutate)
			_, err := Open(p)
			if !errors.Is(err, ErrUnsupportedFeature) {
				t.Fatalf("want ErrUnsupportedFeature, got %v", err)
			}
			var fe *FeatureError
			if !errors.As(err, &fe) || !strings.Contains(err.Error(), c.feature) || !strings.Contains(err.Error(), "upgrade the Attos SDK") {
				t.Fatalf("error must name %q and say to upgrade: %v", c.feature, err)
			}
			if errors.Is(err, ErrCorruptedBinary) {
				t.Fatalf("an intact newer file must not be reported as corrupted: %v", err)
			}
			// also rejected when opening with CRC verification disabled paths and by Validate
			if _, err := OpenWithConfig(p, OpenConfig{}); err == nil {
				t.Fatal("OpenWithConfig accepted an unsupported file")
			}
			p2 := patchedGolden(t, "golden_v2_1000.attos", false, c.mutate)
			if _, err := Open(p2); !errors.Is(err, ErrCorruptedBinary) || errors.Is(err, ErrUnsupportedFeature) {
				t.Fatalf("damaged file must stay corrupted, got %v", err)
			}
		})
	}
}

func TestFutureVersionNamesUpgrade(t *testing.T) {
	p := patchedGolden(t, "golden_v2_1000.attos", true, func(b []byte) { binary.BigEndian.PutUint32(b[4:], 3) })
	_, err := Open(p)
	var ve *VersionError
	if !errors.Is(err, ErrInvalidVersion) || !errors.As(err, &ve) || ve.Version != 3 {
		t.Fatalf("want VersionError{3}, got %v", err)
	}
	if !strings.Contains(err.Error(), "version 3") || !strings.Contains(err.Error(), "upgrade the Attos SDK") {
		t.Fatalf("message must name the version and say to upgrade: %v", err)
	}
}
