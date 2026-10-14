package attos

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
)

type Config struct {
	TelemetryEnabled bool `json:"telemetry_enabled"`
}

const defaultBaseURL = "http://localhost:8080"

// Synchronizer is the data-plane SDK for syncing and querying binary blobs
// locally. It manages the full lifecycle: download, mmap, atomic hot-swap,
// background polling, and graceful shutdown.
//
// All hot-path methods (Get, GetString) are safe for concurrent use. The
// underlying mmap pointer is swapped via atomic.Pointer[Map]; no locks are
// held on the read path.
type Synchronizer struct {
	apiKey       string
	baseURL      string
	cacheDir     string
	datasetID    string
	blobPath     string
	syncInterval time.Duration
	nodeID       string
	logger       *log.Logger

	config atomic.Pointer[Config]
	db     atomic.Pointer[Map]

	ticker *time.Ticker
	cancel context.CancelFunc
	done   chan struct{}
	wg     sync.WaitGroup // tracks the poll() goroutine
	
	openConfig OpenConfig
}

// log emits a message through the injected logger (or log.Default()).
func (s *Synchronizer) log(format string, args ...any) {
	l := s.logger
	if l == nil {
		l = log.Default()
	}
	l.Printf(format, args...)
}

// NewSynchronizer creates and initialises a Synchronizer.
//
// It performs an initial synchronous Sync call so that the first call to Get
// or GetString succeeds immediately after this function returns. Background
// polling is enabled only when WithSyncInterval is provided with a non-zero
// duration.
func NewSynchronizer(ctx context.Context, datasetID string, opts ...Option) (*Synchronizer, error) {
	ctx, cancel := context.WithCancel(ctx)
	s := &Synchronizer{
		datasetID: datasetID,
		baseURL:   defaultBaseURL,
		cacheDir:  "./.attos/cache",
		nodeID:    uuid.New().String(),
		cancel:    cancel,
		done:      make(chan struct{}),
	}
	s.config.Store(&Config{TelemetryEnabled: false}) // Privacy by Default
	for _, opt := range opts {
		opt(s)
	}

	if err := s.Sync(ctx); err != nil {
		return nil, err
	}

	if s.syncInterval > 0 {
		s.ticker = time.NewTicker(s.syncInterval)
		s.wg.Add(1)
		go s.poll(ctx)
	}

	return s, nil
}

func (s *Synchronizer) poll(ctx context.Context) {
	defer s.wg.Done()
	for {
		select {
		case <-s.ticker.C:
			start := time.Now()
			err := s.Sync(ctx)
			latency := time.Since(start)
			s.reportTelemetry(ctx, err, latency)
		case <-ctx.Done():
			return
		case <-s.done:
			return
		}
	}
}

func (s *Synchronizer) reportTelemetry(ctx context.Context, syncErr error, latency time.Duration) {
	cfg := s.config.Load()
	if cfg == nil || !cfg.TelemetryEnabled {
		return // Privacy by Default: do nothing if telemetry is disabled
	}

	crc32Errors := int64(0)
	if syncErr != nil {
		// Example: track CRC32 errors explicitly if defined in errors.go
		if syncErr.Error() == "checksum mismatch" {
			crc32Errors = 1
		}
	}

	nowNano := fmt.Sprintf("%d", time.Now().UnixNano())
	
	// Construct the OTel Gauge for all metrics.
	// We report p50/p95/p99 as the same latency value here since this is a single sync event.
	// The lookups_count and memory_bytes would normally come from internal metrics; 
	// for now we send 0 since sync.go doesn't track reads directly.
	latencyNs := int64(latency.Nanoseconds())

	payload := map[string]any{
		"resourceSpans": []map[string]any{
			{
				"resource": map[string]any{
					"attributes": []map[string]any{
						{"key": "service.name", "value": map[string]any{"stringValue": "attos-go-sdk"}},
						{"key": "attos.dataset_id", "value": map[string]any{"stringValue": s.datasetID}},
						{"key": "attos.node_id", "value": map[string]any{"stringValue": s.nodeID}},
						{"key": "sdk.version", "value": map[string]any{"stringValue": "go-1.0.0"}},
					},
				},
				"scopeMetrics": []map[string]any{
					{
						"scope": map[string]any{"name": "attos-sdk", "version": "1.0.0"},
						"metrics": []map[string]any{
							{
								"name": "attos.lookup.p50_ns",
								"gauge": map[string]any{
									"dataPoints": []map[string]any{{"asInt": latencyNs, "timeUnixNano": nowNano}},
								},
							},
							{
								"name": "attos.lookup.p95_ns",
								"gauge": map[string]any{
									"dataPoints": []map[string]any{{"asInt": latencyNs, "timeUnixNano": nowNano}},
								},
							},
							{
								"name": "attos.lookup.p99_ns",
								"gauge": map[string]any{
									"dataPoints": []map[string]any{{"asInt": latencyNs, "timeUnixNano": nowNano}},
								},
							},
							{
								"name": "attos.lookups_count",
								"gauge": map[string]any{
									"dataPoints": []map[string]any{{"asInt": int64(0), "timeUnixNano": nowNano}},
								},
							},
							{
								"name": "attos.memory_bytes",
								"gauge": map[string]any{
									"dataPoints": []map[string]any{{"asInt": int64(0), "timeUnixNano": nowNano}},
								},
							},
							{
								"name": "attos.crc32_errors",
								"gauge": map[string]any{
									"dataPoints": []map[string]any{{"asInt": crc32Errors, "timeUnixNano": nowNano}},
								},
							},
						},
					},
				},
			},
		},
	}

	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/api/v1/telemetry/report", s.baseURL)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Active-Project-ID", "00000000-0000-0000-0000-000000000000") // Needs real Project ID later, fallback
	req.Header.Set("Content-Type", "application/json")

	// Fire and forget
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		} else {
			s.log("[attos] Telemetry dispatch failed: %v", err)
		}
	}()
}

// Sync downloads the compiled blob from the control plane and hot-swaps it.
// It retries up to 3 times with exponential backoff (100ms → 200ms → 400ms).
func (s *Synchronizer) Sync(ctx context.Context) error {
	var lastErr error
	backoff := 100 * time.Millisecond
	for i := 0; i < 3; i++ {
		lastErr = s.doSync(ctx)
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
			backoff *= 2
		}
	}
	return fmt.Errorf("sync failed after retries: %w", lastErr)
}

func (s *Synchronizer) doSync(ctx context.Context) error {
	if err := os.MkdirAll(s.cacheDir, 0o755); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}

	blobPath := filepath.Join(s.cacheDir, s.datasetID+".nh")
	f, err := os.Create(blobPath)
	if err != nil {
		return fmt.Errorf("create blob file: %w", err)
	}
	defer f.Close()

	if s.openConfig.LocalObjectStore {
		return s.doLocalS3Sync(ctx, f, blobPath)
	}

	url := fmt.Sprintf("%s/api/v1/datasets/sync/%s", s.baseURL, s.datasetID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("X-API-Key", s.apiKey)
	req.Header.Set("X-Attos-Node-ID", s.nodeID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("sync request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusPaymentRequired || resp.StatusCode == http.StatusTooManyRequests {
		s.log("[attos] WARNING: Quota exceeded (%d). Sync paused for 5 minutes. Serving local reads.", resp.StatusCode)
		select {
		case <-time.After(5 * time.Minute):
		case <-ctx.Done():
		}
		return nil
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sync failed (%d): %s", resp.StatusCode, string(body))
	}

	// Read config header for telemetry state
	if configHeader := resp.Header.Get("X-Attos-Config"); configHeader != "" {
		var cfg Config
		if err := json.Unmarshal([]byte(configHeader), &cfg); err == nil {
			s.config.Store(&cfg)
		}
	}

	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("write blob: %w", err)
	}

	return s.openBlob(blobPath)
}

func (s *Synchronizer) openBlob(blobPath string) error {
	m, err := Open(blobPath)
	if err != nil {
		return fmt.Errorf("open mmap db: %w", err)
	}

	s.blobPath = blobPath

	// INVARIANT: swap the new pointer first, then close the old map.
	// This guarantees readers always observe a valid (open) Map reference.
	oldDB := s.db.Swap(m)
	if oldDB != nil {
		_ = oldDB.Close()
	}

	return nil
}

// Get performs an O(1) local lookup against the memory-mapped blob.
// Zero allocations on the hot path.
func (s *Synchronizer) Get(key []byte) ([]byte, error) {
	m := s.db.Load()
	if m == nil {
		return nil, fmt.Errorf("no blob loaded")
	}
	return m.Get(key)
}

// GetString performs an O(1) local lookup returning a zero-copy string.
// The returned string aliases mmap memory and remains valid until Close.
func (s *Synchronizer) GetString(key string) (string, error) {
	m := s.db.Load()
	if m == nil {
		return "", fmt.Errorf("no blob loaded")
	}
	return m.GetString(key)
}

// Close stops background polling and unmaps the memory-mapped blob.
//
// Close blocks until the poll() goroutine has exited, ensuring no
// concurrent reader accesses the mmap region after munmap returns.
// It is safe to call Close exactly once.
func (s *Synchronizer) Close() error {
	// Signal the poll goroutine to stop.
	if s.cancel != nil {
		s.cancel()
	}
	if s.ticker != nil {
		s.ticker.Stop()
		// Signal poll() via the done channel (in case ctx.Done() races).
		// Guard against double-close if Close() is called before ticker init.
		select {
		case <-s.done:
			// already closed
		default:
			close(s.done)
		}
	}

	// Wait for the poll goroutine to exit before unmapping memory.
	// This prevents a potential use-after-free if poll() is mid-Get.
	s.wg.Wait()

	m := s.db.Swap(nil)
	if m != nil {
		return m.Close()
	}
	return nil
}

func (s *Synchronizer) doLocalS3Sync(ctx context.Context, f *os.File, blobPath string) error {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("aws config load: %w", err)
	}

	client := s3.NewFromConfig(cfg)
	key := s.datasetID + ".attos"

	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.openConfig.LocalS3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3 get object: %w", err)
	}
	defer resp.Body.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("write blob: %w", err)
	}

	return s.openBlob(blobPath)
}
