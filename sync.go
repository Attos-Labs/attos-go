package attos

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

const defaultBaseURL = "http://localhost:8080"

// Synchronizer is the data-plane SDK for syncing and querying binary blobs locally.
type Synchronizer struct {
	apiKey       string
	baseURL      string
	cacheDir     string
	datasetID    string
	blobPath     string
	syncInterval time.Duration
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
		done:      make(chan struct{}),
	}
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
			_ = s.Sync() // Errors can be logged or emitted via telemetry
		case <-s.done:
			return
		}
	}
}

// Sync downloads the compiled blob and saves it to local disk, then memory-maps it.
func (s *Synchronizer) Sync() error {
	url := fmt.Sprintf("%s/api/v1/sync/%s", s.baseURL, s.datasetID)

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
