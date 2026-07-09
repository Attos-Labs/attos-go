package attos

import "time"

// Option configures the Attos Synchronizer.
type Option func(*Synchronizer)

// WithBaseURL sets the control plane URL.
func WithBaseURL(url string) Option {
	return func(s *Synchronizer) { s.baseURL = url }
}

// WithCacheDir sets where blobs are stored locally.
func WithCacheDir(dir string) Option {
	return func(s *Synchronizer) { s.cacheDir = dir }
}

// WithAPIKey sets the API key for authentication.
func WithAPIKey(key string) Option {
	return func(s *Synchronizer) { s.apiKey = key }
}

// WithSyncInterval sets the interval for background polling.
func WithSyncInterval(d time.Duration) Option {
	return func(s *Synchronizer) { s.syncInterval = d }
}
