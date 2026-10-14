package attos

import (
	"log"
	"time"
)

// Option configures the Attos Synchronizer.
type Option func(*Synchronizer)

// WithBaseURL sets the control plane base URL.
// Default: "http://localhost:8080".
func WithBaseURL(url string) Option {
	return func(s *Synchronizer) { s.baseURL = url }
}

// WithCacheDir sets the directory used to persist downloaded .nh blobs.
// Default: "./.attos/cache".
func WithCacheDir(dir string) Option {
	return func(s *Synchronizer) { s.cacheDir = dir }
}

// WithAPIKey sets the bearer token sent in X-API-Key on every sync request.
func WithAPIKey(key string) Option {
	return func(s *Synchronizer) { s.apiKey = key }
}

// WithSyncInterval sets the polling interval for background cloud sync.
// A value of 0 disables background polling (manual Sync calls only).
// Default: 0 (disabled).
func WithSyncInterval(d time.Duration) Option {
	return func(s *Synchronizer) { s.syncInterval = d }
}

// WithLogger injects a custom *log.Logger for all operational messages emitted
// by the Synchronizer (sync errors, hot-swap events, telemetry failures).
// If not set, log.Default() is used.
func WithLogger(l *log.Logger) Option {
	return func(s *Synchronizer) { s.logger = l }
}

// WithOpenConfig sets the OpenConfig for the Synchronizer.
func WithOpenConfig(cfg OpenConfig) Option {
	return func(s *Synchronizer) { s.openConfig = cfg }
}
