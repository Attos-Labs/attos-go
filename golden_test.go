package attos_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	attos "github.com/Attos-Labs/attos-go"
)

// goldenDir locates testdata/golden: $GOLDEN_DIR, else the workspace copy
// relative to a main checkout (../testdata/golden) or a track worktree
// (../../../../testdata/golden). Tests skip with a clear message if absent.
func goldenDir(t testing.TB) string {
	t.Helper()
	cands := []string{}
	if d := os.Getenv("GOLDEN_DIR"); d != "" {
		cands = append(cands, d)
	}
	cands = append(cands, "../testdata/golden", "../../../../testdata/golden")
	for _, d := range cands {
		if st, err := os.Stat(filepath.Join(d, "MANIFEST.md")); err == nil && !st.IsDir() {
			abs, _ := filepath.Abs(d)
			return abs
		}
	}
	t.Skipf("golden fixtures not found (set GOLDEN_DIR; tried %v) — SKIPPING golden tests", cands)
	return ""
}

type goldenSuccess struct {
	name, inputSHA, blobSHA string
	rows, bytes             int
}

type goldenError struct {
	name, inputSHA, class string
	line                  int // 0 = no line
}

type goldenNegative struct {
	name, sha, want string
}

var tickRe = regexp.MustCompile("`([0-9a-f]{64})`")

func parseManifest(t testing.TB, dir string) ([]goldenSuccess, []goldenError, []goldenNegative) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "MANIFEST.md"))
	if err != nil {
		t.Fatal(err)
	}
	var succ []goldenSuccess
	var errs []goldenError
	var negs []goldenNegative
	section := ""
	errLineRe := regexp.MustCompile(`^(\w+)(?: \(line (\d+)\))?$`)
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "## Success"):
			section = "s"
			continue
		case strings.HasPrefix(line, "## Error"):
			section = "e"
			continue
		case strings.HasPrefix(line, "## Reader-negative"):
			section = "n"
			continue
		case strings.HasPrefix(line, "## "):
			section = ""
			continue
		}
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "|---") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if cells[0] == "input" || cells[0] == "file" {
			continue
		}
		switch section {
		case "s":
			rows, _ := strconv.Atoi(cells[2])
			bytesN, _ := strconv.Atoi(cells[3])
			succ = append(succ, goldenSuccess{
				name: cells[0], inputSHA: tickRe.FindStringSubmatch(cells[1])[1],
				rows: rows, bytes: bytesN, blobSHA: tickRe.FindStringSubmatch(cells[4])[1],
			})
		case "e":
			m := errLineRe.FindStringSubmatch(cells[2])
			if m == nil {
				t.Fatalf("bad error row %q", line)
			}
			ln, _ := strconv.Atoi(m[2])
			errs = append(errs, goldenError{name: cells[0], inputSHA: tickRe.FindStringSubmatch(cells[1])[1], class: m[1], line: ln})
		case "n":
			negs = append(negs, goldenNegative{name: cells[0], sha: tickRe.FindStringSubmatch(cells[1])[1], want: cells[2]})
		}
	}
	if len(succ) == 0 || len(errs) == 0 || len(negs) == 0 {
		t.Fatalf("manifest parse: %d success, %d error, %d negative rows", len(succ), len(errs), len(negs))
	}
	return succ, errs, negs
}

func sha256Hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func readFileSHA(t testing.TB, path string) ([]byte, string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b, sha256Hex(b)
}

var errClasses = map[string]error{
	"ErrMissingValueColumn": attos.ErrMissingValueColumn,
	"ErrNoDimensions":       attos.ErrNoDimensions,
	"ErrMalformedRow":       attos.ErrMalformedRow,
	"ErrDuplicateKey":       attos.ErrDuplicateKey,
	"ErrLimit":              attos.ErrLimit,
	"ErrBlobTooLarge":       attos.ErrBlobTooLarge,
	"ErrUnplaceable":        attos.ErrUnplaceable,
}

func errLine(err error) int {
	var me *attos.MalformedRowError
	var de *attos.DuplicateKeyError
	var le *attos.LimitError
	switch {
	case errors.As(err, &me):
		return me.Line
	case errors.As(err, &de):
		return de.Line
	case errors.As(err, &le):
		return le.Line
	}
	return 0
}

// TestGoldenBuilder compiles every golden input with the builder and checks the
// blob SHA-256 (success fixtures) or error class + line (error fixtures).
func TestGoldenBuilder(t *testing.T) {
	dir := goldenDir(t)
	succ, errs, _ := parseManifest(t, dir)
	for _, f := range succ {
		t.Run(f.name, func(t *testing.T) {
			in, insha := readFileSHA(t, filepath.Join(dir, "inputs", f.name))
			if insha != f.inputSHA {
				t.Fatalf("input sha %s != manifest %s", insha, f.inputSHA)
			}
			// Streaming API (io.Reader → io.Writer).
			var out bytes.Buffer
			st, err := attos.Compile(context.Background(), bytes.NewReader(in), attos.FormatAuto, &out)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if got := sha256Hex(out.Bytes()); got != f.blobSHA {
				t.Fatalf("blob sha %s != manifest %s (len %d vs %d)", got, f.blobSHA, out.Len(), f.bytes)
			}
			if int(st.Rows) != f.rows || st.Bytes != int64(f.bytes) {
				t.Fatalf("stats rows=%d bytes=%d, manifest rows=%d bytes=%d", st.Rows, st.Bytes, f.rows, f.bytes)
			}
			// File API (BuildFromFile → Compile(path)).
			b := attos.NewBuilder()
			if err := b.BuildFromFile(filepath.Join(dir, "inputs", f.name)); err != nil {
				t.Fatalf("BuildFromFile: %v", err)
			}
			outPath := filepath.Join(t.TempDir(), "out.attos")
			if err := b.CompileV1(outPath); err != nil {
				t.Fatalf("CompileV1(file): %v", err)
			}
			if _, got := readFileSHA(t, outPath); got != f.blobSHA {
				t.Fatalf("file blob sha %s != manifest %s", got, f.blobSHA)
			}
			// Byte-identical with the checked-in expected blob.
			if _, got := readFileSHA(t, filepath.Join(dir, "expected", f.name+".attos")); got != f.blobSHA {
				t.Fatalf("expected/%s.attos sha %s != manifest", f.name, got)
			}
		})
	}
	for _, f := range errs {
		t.Run(f.name, func(t *testing.T) {
			in, insha := readFileSHA(t, filepath.Join(dir, "inputs", f.name))
			if insha != f.inputSHA {
				t.Fatalf("input sha %s != manifest %s", insha, f.inputSHA)
			}
			var out bytes.Buffer
			_, err := attos.Compile(context.Background(), bytes.NewReader(in), attos.FormatAuto, &out)
			if err == nil {
				t.Fatalf("expected %s, got success", f.class)
			}
			if out.Len() != 0 {
				t.Fatalf("%d bytes written despite error", out.Len())
			}
			want := errClasses[f.class]
			if !errors.Is(err, want) {
				t.Fatalf("error %v is not %s", err, f.class)
			}
			if got := errLine(err); got != f.line {
				t.Fatalf("error line %d, want %d (%v)", got, f.line, err)
			}
			t.Logf("%s -> %v", f.name, err)
		})
	}
}

func loadKV(t testing.TB, path string) (keys, vals [][]byte) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4<<20), 8<<20)
	for sc.Scan() {
		line := sc.Text()
		k, v, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("bad kv line %q", line)
		}
		kb, err1 := hex.DecodeString(k)
		vb, err2 := hex.DecodeString(v)
		if err1 != nil || err2 != nil {
			t.Fatalf("bad hex in %s", path)
		}
		keys = append(keys, kb)
		vals = append(vals, vb)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return
}

func loadAbsent(t testing.TB, path string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	if len(data) == 0 {
		return nil
	}
	// One hex key per line; an empty line is the empty key.
	for _, l := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		b, err := hex.DecodeString(l)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

// TestGoldenReader opens every golden success blob with CRC validation and
// resolves every key, checks absent probes, and checks reader-negative fixtures.
func TestGoldenReader(t *testing.T) {
	dir := goldenDir(t)
	succ, _, negs := parseManifest(t, dir)
	for _, f := range succ {
		t.Run(f.name, func(t *testing.T) {
			path := filepath.Join(dir, "expected", f.name+".attos")
			if _, got := readFileSHA(t, path); got != f.blobSHA {
				t.Fatalf("fixture sha mismatch")
			}
			m, err := attos.OpenWithConfig(path, attos.OpenConfig{ValidateChecksum: true})
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer m.Close()
			keys, vals := loadKV(t, filepath.Join(dir, "expected", f.name+".kv"))
			if len(keys) != f.rows {
				t.Fatalf("kv rows %d != manifest %d", len(keys), f.rows)
			}
			if m.Len() != f.rows {
				t.Fatalf("Len() = %d, want %d", m.Len(), f.rows)
			}
			var dst []byte
			for i, k := range keys {
				v, err := m.Get(k)
				if err != nil || !bytes.Equal(v, vals[i]) {
					t.Fatalf("Get(%q) = %q, %v; want %q", k, v, err, vals[i])
				}
				s, err := m.GetString(string(k))
				if err != nil || s != string(vals[i]) {
					t.Fatalf("GetString(%q) mismatch: %v", k, err)
				}
				dst, err = m.GetCopy(k, dst[:0])
				if err != nil || !bytes.Equal(dst, vals[i]) {
					t.Fatalf("GetCopy(%q) mismatch: %v", k, err)
				}
			}
			for _, a := range loadAbsent(t, filepath.Join(dir, "expected", f.name+".absent")) {
				if v, err := m.Get(a); !errors.Is(err, attos.ErrKeyNotFound) {
					t.Fatalf("absent key %q: got %q, %v", a, v, err)
				}
			}
			n := 0
			m.Scan(func(k, v []byte) bool { n++; return true })
			if n != f.rows {
				t.Fatalf("Scan visited %d rows, want %d", n, f.rows)
			}
			t.Logf("%s: %d keys resolved, absent probes ok", f.name, len(keys))
		})
	}
	want := map[string]error{
		"InvalidMagic":     attos.ErrInvalidMagic,
		"InvalidVersion":   attos.ErrInvalidVersion,
		"ChecksumMismatch": attos.ErrChecksumMismatch,
		"Corrupted":        attos.ErrCorruptedBinary,
	}
	for _, f := range negs {
		t.Run(f.name, func(t *testing.T) {
			path := filepath.Join(dir, "expected", f.name)
			if _, got := readFileSHA(t, path); got != f.sha {
				t.Fatalf("fixture sha mismatch")
			}
			class := strings.FieldsFunc(f.want, func(r rune) bool { return r == ' ' || r == '(' })[0]
			if f.name == "bad_version.attos" {
				// The fixture is a v1 body under version 2. Version 2 is a real format now, so the reader
				// rejects it as a malformed v2 file instead of an unknown version.
				class = "Corrupted"
			}
			_, err := attos.OpenWithConfig(path, attos.OpenConfig{ValidateChecksum: true})
			if !errors.Is(err, want[class]) {
				t.Fatalf("%s: got %v, want %s", f.name, err, class)
			}
			if class == "InvalidVersion" {
				var ve *attos.VersionError
				if !errors.As(err, &ve) || ve.Version != 2 {
					t.Fatalf("want VersionError{2}, got %v", err)
				}
			}
			// The validation used by the sync path must reject it as well.
			if err := attos.ValidateFile(path); !errors.Is(err, want[class]) {
				t.Fatalf("ValidateFile: got %v, want %s", err, class)
			}
			t.Logf("%s -> %v", f.name, err)
		})
	}
}
