package attos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Telemetry emitter (CONTRACTS.md §5).
//
// A collector goroutine builds one report per interval from counters that the
// lookup path maintains anyway (striped exit counters, the 1/256-sampled
// latency histogram) — nothing is built or sent on the hot path. Reports go
// through a bounded queue of 64; when it is full the newest report is dropped
// and attos.telemetry_dropped is incremented. A sender goroutine POSTs with
// X-API-Key auth (the project is derived from the key; no Active-Project-ID):
// 5xx/network → up to 3 retries after 1 s, 2 s, 4 s then drop; 4xx except
// 429 → drop; 429 → drop and pause for Retry-After or 60 s. Reports are only
// built while the last sync's X-Attos-Config.telemetry_enabled is true
// (default true until the first sync).

const (
	DefaultTelemetryInterval = 60 * time.Second
	telemetryQueueSize       = 64
	telemetry429Pause        = 60 * time.Second
)

type telemetryEmitter struct {
	s        *Synchronizer
	url      string
	apiKey   string
	queue    chan telemetryReport
	dropped  atomic.Uint64 // reports dropped since the last successful send
	sent     atomic.Uint64
	attempts atomic.Uint64

	buildMu     sync.Mutex // guards lastLookups / lastCRC
	lastLookups uint64
	lastCRC     uint64
	pauseUntil  atomic.Int64 // unix nanos

	retryDelays []time.Duration
}

type telemetryReport struct {
	body           []byte
	droppedCarried uint64
}

func newTelemetryEmitter(s *Synchronizer) *telemetryEmitter {
	base := s.telemetryBaseURL
	if base == "" {
		base = s.baseURL
	}
	key := s.telemetryAPIKey
	if key == "" {
		key = s.apiKey
	}
	return &telemetryEmitter{
		s:           s,
		url:         base + "/api/v1/telemetry/report",
		apiKey:      key,
		queue:       make(chan telemetryReport, telemetryQueueSize),
		retryDelays: []time.Duration{time.Second, 2 * time.Second, 4 * time.Second},
	}
}

func (t *telemetryEmitter) start(wg *sync.WaitGroup) {
	t.lastLookups = t.s.m.Lookups()
	t.lastCRC = t.s.crcErrors.Load()
	wg.Add(2)
	go t.collect(wg)
	go t.send(wg)
}

func (t *telemetryEmitter) collect(wg *sync.WaitGroup) {
	defer wg.Done()
	tick := time.NewTicker(t.s.telemetryInterval)
	defer tick.Stop()
	for {
		select {
		case <-t.s.ctx.Done():
			return
		case <-tick.C:
			t.enqueue(t.build())
		}
	}
}

// enqueue adds a report without blocking; drop-newest on a full queue.
func (t *telemetryEmitter) enqueue(r *telemetryReport) {
	if r == nil {
		return
	}
	select {
	case t.queue <- *r:
	default:
		t.dropped.Add(1 + r.droppedCarried)
	}
}

type otelValue struct {
	StringValue string `json:"stringValue"`
}
type otelAttr struct {
	Key   string    `json:"key"`
	Value otelValue `json:"value"`
}
type otelPoint struct {
	AsInt        int64  `json:"asInt"`
	TimeUnixNano string `json:"timeUnixNano"`
}
type otelMetric struct {
	Name  string `json:"name"`
	Gauge struct {
		DataPoints []otelPoint `json:"dataPoints"`
	} `json:"gauge"`
}
type otelScopeMetrics struct {
	Scope struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"scope"`
	Metrics []otelMetric `json:"metrics"`
}
type otelResourceSpan struct {
	Resource struct {
		Attributes []otelAttr `json:"attributes"`
	} `json:"resource"`
	ScopeMetrics []otelScopeMetrics `json:"scopeMetrics"`
}
type otelPayload struct {
	ResourceSpans []otelResourceSpan `json:"resourceSpans"`
}

func clampInt64(v uint64) int64 {
	if v > 1<<62 {
		return 1 << 62
	}
	return int64(v)
}

// build snapshots the window's counters. It always advances the window, but
// only returns a report when telemetry is enabled.
func (t *telemetryEmitter) build() *telemetryReport {
	t.buildMu.Lock()
	defer t.buildMu.Unlock()
	m := t.s.m
	lookups := m.Lookups()
	dLookups := lookups - t.lastLookups
	if lookups < t.lastLookups {
		dLookups = 0
	}
	t.lastLookups = lookups
	crc := t.s.crcErrors.Load()
	dCRC := crc - t.lastCRC
	t.lastCRC = crc
	lat := m.DrainLatency()
	if !t.s.TelemetryEnabled() {
		return nil
	}
	dropped := t.dropped.Swap(0)
	now := strconv.FormatInt(time.Now().UnixNano(), 10)
	metric := func(name string, v int64) otelMetric {
		var mt otelMetric
		mt.Name = name
		mt.Gauge.DataPoints = []otelPoint{{AsInt: v, TimeUnixNano: now}}
		return mt
	}
	var sm otelScopeMetrics
	sm.Scope.Name = "attos-sdk"
	sm.Scope.Version = Version
	sm.Metrics = []otelMetric{
		metric("attos.lookup.p50_ns", clampInt64(lat.P50)),
		metric("attos.lookup.p95_ns", clampInt64(lat.P95)),
		metric("attos.lookup.p99_ns", clampInt64(lat.P99)),
		metric("attos.lookups_count", clampInt64(dLookups)),
		metric("attos.memory_bytes", m.Size()),
		metric("attos.crc32_errors", clampInt64(dCRC)),
		metric("attos.telemetry_dropped", clampInt64(dropped)),
	}
	var rs otelResourceSpan
	rs.Resource.Attributes = []otelAttr{
		{"attos.dataset_id", otelValue{t.s.stashID}},
		{"attos.node_id", otelValue{t.s.nodeID}},
		{"sdk.version", otelValue{"go-" + Version}},
	}
	rs.ScopeMetrics = []otelScopeMetrics{sm}
	body, err := json.Marshal(otelPayload{ResourceSpans: []otelResourceSpan{rs}})
	if err != nil {
		t.dropped.Add(1 + dropped)
		return nil
	}
	return &telemetryReport{body: body, droppedCarried: dropped}
}

func (t *telemetryEmitter) send(wg *sync.WaitGroup) {
	defer wg.Done()
	ctx := t.s.ctx
	for {
		var r telemetryReport
		select {
		case <-ctx.Done():
			return
		case r = <-t.queue:
		}
		if until := t.pauseUntil.Load(); until > 0 {
			if !t.s.sleep(ctx, time.Until(time.Unix(0, until))) {
				return
			}
		}
		if !t.deliver(ctx, r) {
			t.dropped.Add(1 + r.droppedCarried)
		}
	}
}

// deliver POSTs one report with the §5 retry policy; false means dropped.
func (t *telemetryEmitter) deliver(ctx context.Context, r telemetryReport) bool {
	for attempt := 0; ; attempt++ {
		code, retryAfter, err := t.post(ctx, r.body)
		switch {
		case err == nil && code >= 200 && code < 300:
			t.sent.Add(1)
			return true
		case err == nil && code == http.StatusTooManyRequests:
			d := retryAfter
			if d <= 0 {
				d = telemetry429Pause
			}
			t.pauseUntil.Store(time.Now().Add(d).UnixNano())
			return false
		case err == nil && code >= 400 && code < 500:
			t.s.log("[attos] telemetry rejected: HTTP %d (dropped)", code)
			return false
		}
		// 5xx or network error.
		if attempt >= len(t.retryDelays) || ctx.Err() != nil {
			t.s.log("[attos] telemetry send failed after %d attempts (dropped): code=%d err=%v", attempt+1, code, err)
			return false
		}
		if !t.s.sleep(ctx, t.retryDelays[attempt]) {
			return false
		}
	}
}

func (t *telemetryEmitter) post(ctx context.Context, body []byte) (int, time.Duration, error) {
	t.attempts.Add(1)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", t.apiKey)
	req.Header.Set("User-Agent", "attos-go/"+Version)
	resp, err := t.s.httpClient.Do(req)
	if err != nil {
		return 0, 0, err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	return resp.StatusCode, parseRetryAfter(resp.Header.Get("Retry-After")), nil
}

// TelemetryStats exposes emitter counters (for tests and diagnostics).
type TelemetryStats struct {
	Sent, Attempts, DroppedPending uint64
	Queued                         int
}

// TelemetryStats returns the emitter counters (zero if telemetry is off).
func (s *Synchronizer) TelemetryStats() TelemetryStats {
	if s.tel == nil {
		return TelemetryStats{}
	}
	return TelemetryStats{Sent: s.tel.sent.Load(), Attempts: s.tel.attempts.Load(),
		DroppedPending: s.tel.dropped.Load(), Queued: len(s.tel.queue)}
}

// FlushTelemetry builds and enqueues a report immediately (testing/diagnostics).
func (s *Synchronizer) FlushTelemetry() error {
	if s.tel == nil {
		return errors.New("attos: telemetry disabled")
	}
	s.tel.enqueue(s.tel.build())
	return nil
}
