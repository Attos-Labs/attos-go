package attos

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

type Config struct {
	TelemetryEnabled bool `json:"telemetry_enabled"`
}

const defaultBaseURL = "http://localhost:8080"

// Synchronizer is the data-plane SDK for syncing and querying binary blobs locally.
type Synchronizer struct {
	apiKey       string
	baseURL      string
	cacheDir     string
	datasetID    string
	blobPath     string
	syncInterval time.Duration
	nodeID       string
	config       atomic.Pointer[Config]
	db           atomic.Pointer[Map]
	ticker       *time.Ticker
	done         chan struct{}
}

// NewSynchronizer creates a Synchronizer authenticated with the given API key.
func NewSynchronizer(datasetID string, opts ...Option) (*Synchronizer, error) {
	s := &Synchronizer{
		datasetID: datasetID,
		baseURL:   defaultBaseURL,
		cacheDir:  "./.attos/cache",
		nodeID:    fmt.Sprintf("node-%d", time.Now().UnixNano()), // Unique ID for this node
		done:      make(chan struct{}),
	}
	s.config.Store(&Config{TelemetryEnabled: false}) // Privacy by Default
	for _, opt := range opts {
		opt(s)
	}

	if err := s.Sync(); err != nil {
		return nil, err
	}

	if s.syncInterval > 0 {
		s.ticker = time.NewTicker(s.syncInterval)
		go s.poll()
	}

	return s, nil
}

func (s *Synchronizer) poll() {
	for {
		select {
		case <-s.ticker.C:
			start := time.Now()
			err := s.Sync()
			latency := time.Since(start)
			s.reportTelemetry(err, latency)
		case <-s.done:
			return
		}
	}
}

func (s *Synchronizer) reportTelemetry(syncErr error, latency time.Duration) {
	cfg := s.config.Load()
	if cfg == nil || !cfg.TelemetryEnabled {
		return // Privacy by Default: do nothing if telemetry is disabled
	}

	status := "sync_success"
	if syncErr != nil {
		status = "sync_failure"
	}

	payload := map[string]any{
		"dataset_id": s.datasetID,
		"node_id":    s.nodeID,
		"status":     status,
		"latency_ms": int32(latency.Milliseconds()),
	}

	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/api/v1/telemetry/report", s.baseURL)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Active-Org-ID", "internal-sdk") // In a real SDK, we'd pass the actual org ID or derive it from the API key
	
	// Fire and forget
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		} else {
			log.Printf("[Attos SDK] Telemetry dispatch failed: %v", err)
		}
	}()
}

// Sync downloads the compiled blob and saves it to local disk, then memory-maps it.
func (s *Synchronizer) Sync() error {
	url := fmt.Sprintf("%s/api/v1/datasets/sync/%s", s.baseURL, s.datasetID)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("X-API-Key", s.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("sync request: %w", err)
	}
	defer resp.Body.Close()

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

	if err := os.MkdirAll(s.cacheDir, 0o755); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}

	blobPath := filepath.Join(s.cacheDir, s.datasetID+".nh")
	f, err := os.Create(blobPath)
	if err != nil {
		return fmt.Errorf("create blob file: %w", err)
	}
	defer f.Close()

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

	oldDB := s.db.Swap(m)
	if oldDB != nil {
		_ = oldDB.Close()
	}

	return nil
}

// Get performs an O(1) local lookup against the memory-mapped blob.
func (s *Synchronizer) Get(key []byte) ([]byte, error) {
	m := s.db.Load()
	if m == nil {
		return nil, fmt.Errorf("no blob loaded")
	}
	return m.Get(key)
}

// GetString performs an O(1) local lookup returning a string.
func (s *Synchronizer) GetString(key string) (string, error) {
	m := s.db.Load()
	if m == nil {
		return "", fmt.Errorf("no blob loaded")
	}
	return m.GetString(key)
}

// Close unmaps the database and stops polling.
func (s *Synchronizer) Close() error {
	if s.ticker != nil {
		s.ticker.Stop()
		close(s.done)
	}
	m := s.db.Swap(nil)
	if m != nil {
		return m.Close()
	}
	return nil
}
