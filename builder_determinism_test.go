package attos_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"

	attos "github.com/Attos-Labs/attos-go"
)

func randomCSV(r *rand.Rand, rows int) (header string, lines []string) {
	header = "region,attos_value,id"
	seen := map[string]bool{}
	for len(lines) < rows {
		id := fmt.Sprintf("k%d", r.IntN(rows*10))
		reg := []string{"eu", "us", "ap"}[r.IntN(3)]
		if seen[id+"|"+reg] {
			continue
		}
		seen[id+"|"+reg] = true
		val := fmt.Sprintf("\"v,%d\"\"q\"\"\"", r.Uint64())
		lines = append(lines, reg+","+val+","+id)
	}
	return
}

func compileString(t *testing.T, s string) []byte {
	t.Helper()
	var out bytes.Buffer
	if _, err := attos.Compile(context.Background(), strings.NewReader(s), attos.FormatAuto, &out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// TestBuilderDeterminism: output is a pure function of the row set — it does
// not depend on input order or GOMAXPROCS.
func TestBuilderDeterminism(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for _, rows := range []int{1, 2, 17, 500, 5000} {
		header, lines := randomCSV(r, rows)
		base := compileString(t, header+"\n"+strings.Join(lines, "\n")+"\n")
		want := sha256.Sum256(base)
		for trial := 0; trial < 3; trial++ {
			r.Shuffle(len(lines), func(i, j int) { lines[i], lines[j] = lines[j], lines[i] })
			for _, procs := range []int{1, runtime.NumCPU()} {
				old := runtime.GOMAXPROCS(procs)
				got := sha256.Sum256(compileString(t, header+"\r\n"+strings.Join(lines, "\r\n")))
				runtime.GOMAXPROCS(old)
				if got != want {
					t.Fatalf("rows=%d trial=%d procs=%d: output differs after shuffle", rows, trial, procs)
				}
			}
		}
	}
}

func TestBuilderErrorsLeaveNoOutput(t *testing.T) {
	cases := map[string]error{
		"id,attos_value\na,1\na,2\n":        attos.ErrDuplicateKey,
		"id,attos_value\na,\"x\n":           attos.ErrMalformedRow,
		"x\n1\n":                            attos.ErrMissingValueColumn,
		"{\"attos_value\":\"1\"}\n":         attos.ErrNoDimensions,
		"{\"k\":\"a\",\"attos_value\":[1]}": attos.ErrMalformedRow,
	}
	for in, want := range cases {
		var out bytes.Buffer
		_, err := attos.Compile(context.Background(), strings.NewReader(in), attos.FormatAuto, &out)
		if !errors.Is(err, want) || out.Len() != 0 {
			t.Errorf("%q: err=%v out=%d, want %v and no output", in, err, out.Len(), want)
		}
	}
}

// A parse error later in the file takes precedence over an earlier duplicate
// (reference implementation semantics), and an earlier duplicate beats a
// later limit violation.
func TestBuilderErrorPrecedence(t *testing.T) {
	in := "id,attos_value\na,1\na,2\nb,\"bad\n"
	_, err := attos.Compile(context.Background(), strings.NewReader(in), attos.FormatAuto, &bytes.Buffer{})
	var me *attos.MalformedRowError
	if !errors.As(err, &me) || me.Line != 4 {
		t.Fatalf("want MalformedRowError line 4, got %v", err)
	}
	in = "id,attos_value\na,1\na,2\n" + strings.Repeat("k", 70000) + ",1\n"
	_, err = attos.Compile(context.Background(), strings.NewReader(in), attos.FormatAuto, &bytes.Buffer{})
	var de *attos.DuplicateKeyError
	if !errors.As(err, &de) || de.Line != 3 || de.FirstLine != 2 {
		t.Fatalf("want DuplicateKeyError 2/3, got %v", err)
	}
	in = "id,attos_value\n" + strings.Repeat("k", 70000) + ",1\na,1\na,2\n"
	_, err = attos.Compile(context.Background(), strings.NewReader(in), attos.FormatAuto, &bytes.Buffer{})
	var le *attos.LimitError
	if !errors.As(err, &le) || le.Line != 2 || le.Field != "key" {
		t.Fatalf("want LimitError line 2, got %v", err)
	}
}

func TestBuilderCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	_, err := attos.Compile(ctx, strings.NewReader("id,attos_value\na,1\n"), attos.FormatAuto, &out)
	if !errors.Is(err, context.Canceled) || out.Len() != 0 {
		t.Fatalf("want context.Canceled and no output, got %v (%d bytes)", err, out.Len())
	}
}

// Builder rows accumulate across BuildFrom calls; a failed call leaves the
// builder unchanged.
func TestBuilderRollback(t *testing.T) {
	b := attos.NewBuilder()
	if err := b.BuildFrom(context.Background(), strings.NewReader("id,attos_value\na,1\n"), attos.FormatCSV); err != nil {
		t.Fatal(err)
	}
	if err := b.BuildFrom(context.Background(), strings.NewReader("id,attos_value\nb,1\na,2\n"), attos.FormatCSV); !errors.Is(err, attos.ErrDuplicateKey) {
		t.Fatalf("want duplicate across calls, got %v", err)
	}
	if b.Len() != 1 {
		t.Fatalf("rollback failed: Len=%d", b.Len())
	}
	var o1, o2 bytes.Buffer
	if _, err := b.CompileTo(&o1); err != nil {
		t.Fatal(err)
	}
	if _, err := attos.Compile(context.Background(), strings.NewReader("id,attos_value\na,1\n"), attos.FormatAuto, &o2); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(o1.Bytes(), o2.Bytes()) {
		t.Fatal("builder state after rollback differs")
	}
}
