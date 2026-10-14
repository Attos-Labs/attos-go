package attos

import (
	"log"
	"sync"
	"testing"
)

type tWriter struct {
	mu   sync.Mutex
	t    testing.TB
	done bool
}

func (w *tWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.done {
		w.t.Logf("%s", p)
	}
	return len(p), nil
}

// testLogger routes SDK log output to t.Log until the test completes.
func testLogger(t testing.TB) *log.Logger {
	w := &tWriter{t: t}
	t.Cleanup(func() { w.mu.Lock(); w.done = true; w.mu.Unlock() })
	return log.New(w, "", log.Lmicroseconds)
}
