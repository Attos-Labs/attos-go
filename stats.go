package attos

import (
	"math/bits"
	"sync/atomic"
	"time"
)

// latencyHistogram is a lock-free log-linear histogram of sampled lookup
// latencies (8 sub-buckets per power of two, ≤ 12.5% relative error).
// Recording is one atomic add; it is only touched on sampled lookups.
type latencyHistogram struct {
	counts [64 * 8]atomic.Uint64
}

func histIndex(ns uint64) int {
	if ns < 8 {
		return int(ns)
	}
	e := 63 - bits.LeadingZeros64(ns) // ≥ 3
	sub := (ns >> (uint(e) - 3)) & 7
	return (e-2)*8 + int(sub)
}

// histUpper returns an upper bound (inclusive) of the values in bucket i.
func histUpper(i int) uint64 {
	if i < 8 {
		return uint64(i)
	}
	e := i/8 + 2
	sub := uint64(i % 8)
	return (8+sub+1)<<(uint(e)-3) - 1
}

func (h *latencyHistogram) record(d time.Duration) {
	ns := uint64(0)
	if d > 0 {
		ns = uint64(d)
	}
	i := histIndex(ns)
	if i >= len(h.counts) {
		i = len(h.counts) - 1
	}
	h.counts[i].Add(1)
}

// LatencySnapshot summarises sampled lookup latencies.
type LatencySnapshot struct {
	Samples            uint64
	P50, P95, P99, Max uint64 // nanoseconds
}

// drain atomically takes and resets the histogram, returning percentiles.
func (h *latencyHistogram) drain() LatencySnapshot {
	var c [len(h.counts)]uint64
	var total uint64
	for i := range h.counts {
		c[i] = h.counts[i].Swap(0)
		total += c[i]
	}
	s := LatencySnapshot{Samples: total}
	if total == 0 {
		return s
	}
	q := func(p float64) uint64 {
		rank := uint64(p*float64(total) + 0.5)
		if rank < 1 {
			rank = 1
		}
		var acc uint64
		for i, n := range c {
			acc += n
			if acc >= rank {
				return histUpper(i)
			}
		}
		return histUpper(len(c) - 1)
	}
	s.P50, s.P95, s.P99 = q(0.50), q(0.95), q(0.99)
	for i := len(c) - 1; i >= 0; i-- {
		if c[i] > 0 {
			s.Max = histUpper(i)
			break
		}
	}
	return s
}

// DrainLatency returns sampled lookup-latency percentiles since the previous
// call and resets the histogram (used by the telemetry reporter).
func (m *Map) DrainLatency() LatencySnapshot { return m.lat.drain() }
