package attos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
)

// Version is the attos-go SDK version reported in User-Agent and telemetry.
const Version = "1.1.0"

// Sync protocol constants (CONTRACTS.md §4). The default poll interval is the
// same in the Go, Rust and Java SDKs.
const (
	DefaultSyncInterval = 30 * time.Second
	QuotaPause          = 5 * time.Minute
	MaxRetryAfter       = time.Hour
	maxTransientBackoff = 5 * time.Minute
)

// Config is the server-provided SDK configuration (X-Attos-Config header).
type Config struct {
	TelemetryEnabled bool `json:"telemetry_enabled"`
}

const defaultBaseURL = "http://localhost:8080"

// StatusError is returned by Sync for a non-200/304 HTTP response.
type StatusError struct {
	Code       int
	RetryAfter time.Duration // parsed Retry-After for 402/429 (0 if absent)
	Body       string
}

func (e *StatusError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("attos sync: HTTP %d: %s", e.Code, e.Body)
	}
	return fmt.Sprintf("attos sync: HTTP %d", e.Code)
}

// Quota reports whether the error is a node/quota limit (402 or 429).
func (e *StatusError) Quota() bool {
	return e.Code == http.StatusPaymentRequired || e.Code == http.StatusTooManyRequests
}

// ErrTruncatedDownload is returned when the body is shorter than Content-Length.
var ErrTruncatedDownload = errors.New("attos sync: truncated download")

// Synchronizer is the data-plane SDK for syncing and querying binary blobs
// locally. It manages the full lifecycle: download, validation, mmap, atomic
// hot-swap, background polling with back-off, telemetry and shutdown.
//
// Lookups (Get, GetString, GetCopy, View) are lock-free and allocation-free;
// see Map for the retirement scheme and the zero-copy validity window.
type Synchronizer struct {
	apiKey       string
	baseURL      string
	cacheDir     string
	datasetID    string
	syncInterval time.Duration
	nodeID       string
	logger       *log.Logger
	httpClient   *http.Client
	openConfig   OpenConfig

	telemetryInterval time.Duration
	telemetryBaseURL  string
	telemetryAPIKey   string
	telemetryOff      bool

	config      atomic.Pointer[Config]
	m           *Map
	datasetName atomic.Pointer[string]

	syncMu       sync.Mutex // serialises Sync calls
	etag         string     // guarded by syncMu
	failures     int        // consecutive transient failures (poll loop only)
	loggedStatus map[int]bool

	crcErrors  atomic.Uint64 // checksum failures of downloaded blobs
	syncOK     atomic.Uint64
	syncFailed atomic.Uint64

	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	closeErr error
	closed   sync.Once
	tel      *telemetryEmitter

	// Test hook: overrides time.Sleep-style waits in the poll loop.
	sleep func(ctx context.Context, d time.Duration) bool
}

// log emits a message through the injected logger (or log.Default()).
func (s *Synchronizer) log(format string, args ...any) {
	l := s.logger
	if l == nil {
		l = log.Default()
	}
	l.Printf(format, args...)
}

// NewSynchronizer creates and initialises a Synchronizer for datasetID.
//
// It first loads a previously synced blob from the cache directory (if one
// exists and validates), then performs an initial Sync. If that sync fails but
// a cached blob is loaded, the error is logged and the cached data is served;
// with no data at all the error is returned. Background polling runs every
// WithSyncInterval (default DefaultSyncInterval; 0 disables polling).
func NewSynchronizer(ctx context.Context, datasetID string, opts ...Option) (*Synchronizer, error) {
	if datasetID == "" {
		return nil, errors.New("attos: datasetID is required")
	}
	s := &Synchronizer{
		datasetID:         datasetID,
		baseURL:           defaultBaseURL,
		cacheDir:          "./.attos/cache",
		nodeID:            uuid.New().String(),
		syncInterval:      DefaultSyncInterval,
		httpClient:        &http.Client{Timeout: 10 * time.Minute},
		telemetryInterval: DefaultTelemetryInterval,
		loggedStatus:      map[int]bool{},
		sleep:             sleepCtx,
	}
	// Telemetry is on by default until the first sync says otherwise (§5).
	s.config.Store(&Config{TelemetryEnabled: true})
	for _, opt := range opts {
		opt(s)
	}
	if len(s.nodeID) == 0 || len(s.nodeID) > 128 {
		return nil, fmt.Errorf("attos: node ID must be 1-128 characters, got %d", len(s.nodeID))
	}
	s.m = newEmptyMap(s.openConfig)
	s.ctx, s.cancel = context.WithCancel(context.Background())

	if err := os.MkdirAll(s.cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	s.removeStaleTemps()
	if err := s.loadCached(); err != nil {
		s.log("[attos] cached blob for %s not usable: %v", s.datasetID, err)
	}

	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = s.Sync(ctx); err == nil {
			break
		}
		var se *StatusError
		if errors.As(err, &se) || ctx.Err() != nil {
			break // non-transient: do not hammer the server
		}
		if !s.sleep(ctx, time.Duration(100<<attempt)*time.Millisecond) {
			break
		}
	}
	if err != nil {
		if s.m.cur.Load() == nil {
			s.cancel()
			s.m.Close()
			return nil, fmt.Errorf("attos: initial sync failed: %w", err)
		}
		s.log("[attos] initial sync failed, serving cached blob: %v", err)
	}

	if s.syncInterval > 0 {
		s.wg.Add(1)
		go s.poll(err)
	}
	if !s.telemetryOff && s.telemetryInterval > 0 {
		s.tel = newTelemetryEmitter(s)
		s.tel.start(&s.wg)
	}
	return s, nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Synchronizer) livePath() string { return filepath.Join(s.cacheDir, s.datasetID+".attos") }
func (s *Synchronizer) etagPath() string { return filepath.Join(s.cacheDir, s.datasetID+".etag") }
func (s *Synchronizer) tempPattern() string {
	return "." + s.datasetID + ".attos.download-*"
}

func (s *Synchronizer) removeStaleTemps() {
	matches, _ := filepath.Glob(filepath.Join(s.cacheDir, s.tempPattern()))
	for _, p := range matches {
		os.Remove(p)
	}
}

// loadCached publishes a previously synced blob, if any.
func (s *Synchronizer) loadCached() error {
	p := s.livePath()
	if _, err := os.Stat(p); err != nil {
		return nil
	}
	mp, err := openMapping(p, s.openConfig, true)
	if err != nil {
		return err
	}
	if err := s.m.publish(mp); err != nil {
		mp.unmap()
		return err
	}
	if b, err := os.ReadFile(s.etagPath()); err == nil {
		s.syncMu.Lock()
		s.etag = string(b)
		s.syncMu.Unlock()
	}
	return nil
}

// poll runs the background sync loop with the CONTRACTS §4 back-off rules.
func (s *Synchronizer) poll(initialErr error) {
	defer s.wg.Done()
	delay := s.nextDelay(initialErr)
	for {
		if !s.sleep(s.ctx, delay) {
			return
		}
		err := s.Sync(s.ctx)
		if s.ctx.Err() != nil {
			return
		}
		delay = s.nextDelay(err)
	}
}

// nextDelay maps a sync outcome to the wait before the next attempt.
func (s *Synchronizer) nextDelay(err error) time.Duration {
	if err == nil {
		s.failures = 0
		return s.syncInterval
	}
	var se *StatusError
	if errors.As(err, &se) {
		if se.Quota() {
			d := QuotaPause
			if se.RetryAfter > 0 {
				d = min(se.RetryAfter, MaxRetryAfter)
			}
			s.log("[attos] WARNING: quota/node limit (%d). Sync paused for %v. Serving local reads.", se.Code, d)
			return d
		}
		if se.Code >= 400 && se.Code < 500 {
			if !s.loggedStatus[se.Code] {
				s.loggedStatus[se.Code] = true
				s.log("[attos] sync rejected (%v); keeping current data, retrying every %v", se, s.syncInterval)
			}
			s.failures = 0
			return s.syncInterval
		}
	}
	// Transient (5xx, network, truncated body, validation failure).
	d := transientBackoff(s.failures, s.syncInterval)
	s.failures++
	s.log("[attos] sync failed (%v); keeping current data, retrying in %v", err, d.Round(time.Millisecond))
	return d
}

// transientBackoff is 1 s × 2^k with ±20% jitter, capped at min(interval, 5 min).
func transientBackoff(k int, interval time.Duration) time.Duration {
	limit := maxTransientBackoff
	if interval > 0 && interval < limit {
		limit = interval
	}
	if k > 20 {
		k = 20
	}
	d := time.Second << k
	if d > limit {
		d = limit
	}
	jitter := 0.8 + 0.4*rand.Float64()
	return time.Duration(float64(d) * jitter)
}

func parseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// Sync performs one sync attempt: conditional GET (If-None-Match), and on 200
// download → fsync → validate → rename → mmap → atomic publish. On any failure
// the current map keeps serving. It does not sleep or retry; the background
// loop applies the back-off policy.
func (s *Synchronizer) Sync(ctx context.Context) error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	var err error
	if s.openConfig.LocalObjectStore {
		err = s.syncLocalS3(ctx)
	} else {
		err = s.syncHTTP(ctx)
	}
	if err != nil {
		s.syncFailed.Add(1)
	} else {
		s.syncOK.Add(1)
	}
	return err
}

func (s *Synchronizer) syncHTTP(ctx context.Context) error {
	url := fmt.Sprintf("%s/api/v1/datasets/sync/%s", s.baseURL, s.datasetID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("X-API-Key", s.apiKey)
	req.Header.Set("X-Attos-Node-ID", s.nodeID)
	req.Header.Set("User-Agent", "attos-go/"+Version)
	if s.etag != "" && s.m.cur.Load() != nil {
		req.Header.Set("If-None-Match", s.etag)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sync request: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		return nil
	case resp.StatusCode != http.StatusOK:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		se := &StatusError{Code: resp.StatusCode, Body: string(body)}
		if se.Quota() {
			se.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
		}
		return se
	}

	if err := s.installFrom(resp.Body, resp.ContentLength); err != nil {
		return err
	}
	s.etag = resp.Header.Get("ETag")
	s.persistETag()
	s.applyConfigHeader(resp.Header.Get("X-Attos-Config"))
	if name := resp.Header.Get("X-Dataset-Name"); name != "" {
		s.datasetName.Store(&name)
	}
	return nil
}

func (s *Synchronizer) applyConfigHeader(h string) {
	if h == "" {
		return
	}
	var cfg Config
	if err := json.Unmarshal([]byte(h), &cfg); err == nil {
		s.config.Store(&cfg)
	} else {
		s.log("[attos] ignoring malformed X-Attos-Config: %v", err)
	}
}

func (s *Synchronizer) persistETag() {
	p := s.etagPath()
	if s.etag == "" {
		os.Remove(p)
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(s.etag), 0o644); err == nil {
		os.Rename(tmp, p)
	}
}

// installFrom implements the CONTRACTS §4 swap invariant for one blob.
func (s *Synchronizer) installFrom(body io.Reader, contentLength int64) error {
	tmp, err := os.CreateTemp(s.cacheDir, s.tempPattern())
	if err != nil {
		return fmt.Errorf("create temp blob: %w", err)
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	n, err := io.Copy(tmp, body)
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("%w: %v", ErrTruncatedDownload, err)
		}
		return fmt.Errorf("download blob: %w", err)
	}
	if contentLength >= 0 && n != contentLength {
		return fmt.Errorf("%w: got %d of %d bytes", ErrTruncatedDownload, n, contentLength)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("fsync blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close blob: %w", err)
	}
	// Validate the complete file (magic, version, flag, size arithmetic, CRC)
	// before it can become visible.
	mp, err := openMapping(tmpName, s.openConfig, true)
	if err != nil {
		if errors.Is(err, ErrChecksumMismatch) {
			s.crcErrors.Add(1)
		}
		return fmt.Errorf("downloaded blob rejected: %w", err)
	}
	live := s.livePath()
	if err := os.Rename(tmpName, live); err != nil {
		mp.unmap()
		return fmt.Errorf("rename blob: %w", err)
	}
	keep = true
	if d, err := os.Open(s.cacheDir); err == nil {
		d.Sync()
		d.Close()
	}
	if err := s.m.publish(mp); err != nil {
		mp.unmap()
		return err
	}
	return nil
}

// statusCoder matches AWS SDK response errors (smithy-go ResponseError).
type statusCoder interface{ HTTPStatusCode() int }

func (s *Synchronizer) syncLocalS3(ctx context.Context) error {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("aws config load: %w", err)
	}
	client := s3.NewFromConfig(cfg)
	in := &s3.GetObjectInput{
		Bucket: aws.String(s.openConfig.LocalS3Bucket),
		Key:    aws.String(s.datasetID + ".attos"),
	}
	if s.etag != "" && s.m.cur.Load() != nil {
		in.IfNoneMatch = aws.String(s.etag)
	}
	resp, err := client.GetObject(ctx, in)
	if err != nil {
		var sc statusCoder
		if errors.As(err, &sc) {
			if sc.HTTPStatusCode() == http.StatusNotModified {
				return nil
			}
			return &StatusError{Code: sc.HTTPStatusCode(), Body: err.Error()}
		}
		return fmt.Errorf("s3 get object: %w", err)
	}
	defer resp.Body.Close()
	cl := int64(-1)
	if resp.ContentLength != nil {
		cl = *resp.ContentLength
	}
	if err := s.installFrom(resp.Body, cl); err != nil {
		return err
	}
	s.etag = aws.ToString(resp.ETag)
	s.persistETag()
	return nil
}

// Map returns the hot-swappable map handle served by this Synchronizer.
func (s *Synchronizer) Map() *Map { return s.m }

// NodeID returns the X-Attos-Node-ID sent by this process.
func (s *Synchronizer) NodeID() string { return s.nodeID }

// ETag returns the ETag of the currently loaded blob ("" if unknown).
func (s *Synchronizer) ETag() string {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	return s.etag
}

// DatasetName returns the last X-Dataset-Name received ("" if none).
func (s *Synchronizer) DatasetName() string {
	if p := s.datasetName.Load(); p != nil {
		return *p
	}
	return ""
}

// TelemetryEnabled reports the server-provided telemetry switch.
func (s *Synchronizer) TelemetryEnabled() bool {
	c := s.config.Load()
	return c == nil || c.TelemetryEnabled
}

// Get performs an O(1) lock-free, allocation-free lookup. The returned slice
// aliases the mapping (validity window: see Map). Use GetCopy to keep it.
func (s *Synchronizer) Get(key []byte) ([]byte, error) { return s.m.Get(key) }

// GetString is the zero-copy string variant of Get.
func (s *Synchronizer) GetString(key string) (string, error) { return s.m.GetString(key) }

// GetCopy appends the value for key to dst (safe to keep across swaps).
func (s *Synchronizer) GetCopy(key, dst []byte) ([]byte, error) { return s.m.GetCopy(key, dst) }

// View calls fn with a zero-copy value while the mapping is pinned.
func (s *Synchronizer) View(key []byte, fn func([]byte)) error { return s.m.View(key, fn) }

// Close stops background polling and telemetry, waits for them to exit, then
// closes the map (waiting for in-flight lookups and unmapping every region).
// It is safe to call more than once.
func (s *Synchronizer) Close() error {
	s.closed.Do(func() {
		s.cancel()
		s.wg.Wait()
		s.closeErr = s.m.Close()
	})
	return s.closeErr
}
