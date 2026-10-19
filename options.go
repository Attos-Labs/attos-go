package attos

import (
	"log"
	"net/http"
	"time"
)

// Option configures the Attos Synchronizer.
type Option func(*Synchronizer)

// WithBaseURL sets the control plane base URL.
// Default: "http://localhost:8080".
func WithBaseURL(url string) Option {
	return func(s *Synchronizer) { s.baseURL = url }
}

// WithCacheDir sets the directory holding the live blob (<datasetID>.attos),
// its ETag and in-progress downloads (always in the same directory, so the
// final rename is atomic). Default: "./.attos/cache".
func WithCacheDir(dir string) Option {
	return func(s *Synchronizer) { s.cacheDir = dir }
}

// WithAPIKey sets the API key sent in X-API-Key on sync and telemetry requests.
func WithAPIKey(key string) Option {
	return func(s *Synchronizer) { s.apiKey = key }
}

// WithSyncInterval sets the background polling interval.
// A value of 0 disables background polling (manual Sync calls only).
// Default: DefaultSyncInterval (30 s), identical across the Go, Rust and Java SDKs.
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

// WithNodeID overrides the X-Attos-Node-ID (1–128 chars). Default: the
// ATTOS_NODE_ID environment variable, else the hostname, else a random UUIDv4.
//
// Active nodes are counted (and billed) by distinct node ID, so every process
// on one host counts as one node by default; containers sharing a hostname are
// one node too. Set a distinct ID per process if they should count separately.
func WithNodeID(id string) Option {
	return func(s *Synchronizer) { s.nodeID = id }
}

// WithCompression controls whether syncs ask for the gzip copy of the blob
// (Accept-Encoding: gzip; default true). Compression is transport-only: the
// stored and memory-mapped blob is always the v1 file.
func WithCompression(on bool) Option {
	return func(s *Synchronizer) { s.noGzip = !on }
}

// WithHTTPClient sets the HTTP client used for sync and telemetry.
func WithHTTPClient(c *http.Client) Option {
	return func(s *Synchronizer) { s.httpClient = c }
}

// WithTelemetryInterval sets the telemetry report interval (default 60 s).
// 0 disables the telemetry emitter.
func WithTelemetryInterval(d time.Duration) Option {
	return func(s *Synchronizer) { s.telemetryInterval = d }
}

// WithTelemetryBaseURL sends telemetry to a different base URL than sync.
func WithTelemetryBaseURL(url string) Option {
	return func(s *Synchronizer) { s.telemetryBaseURL = url }
}

// WithTelemetryAPIKey uses a different API key for telemetry than for sync.
func WithTelemetryAPIKey(key string) Option {
	return func(s *Synchronizer) { s.telemetryAPIKey = key }
}

// WithTelemetryDisabled turns the telemetry emitter off regardless of the
// server's X-Attos-Config.
func WithTelemetryDisabled() Option {
	return func(s *Synchronizer) { s.telemetryOff = true }
}
