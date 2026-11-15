package attos

import (
	"context"
	"testing"
)

// The Synchronizer pre-faults new mappings by default; the opt-out and explicit OpenConfig choices are honoured.
func TestSynchronizerWarmUpDefault(t *testing.T) {
	cases := []struct {
		name string
		opts []Option
		want OpenConfig
	}{
		{"default", nil, OpenConfig{WarmUp: true}},
		{"opt out", []Option{WithWarmUp(false)}, OpenConfig{}},
		{"explicit on", []Option{WithWarmUp(true)}, OpenConfig{WarmUp: true}},
		{"explicit index warm-up is kept", []Option{WithOpenConfig(OpenConfig{Warm: WarmIndex})}, OpenConfig{Warm: WarmIndex}},
		{"explicit full warm-up is kept", []Option{WithOpenConfig(OpenConfig{Warm: WarmFull})}, OpenConfig{Warm: WarmFull}},
		{"explicit WarmUp config survives the opt-out", []Option{WithOpenConfig(OpenConfig{WarmUp: true}), WithWarmUp(false)}, OpenConfig{WarmUp: true}},
		{"other settings survive", []Option{WithOpenConfig(OpenConfig{PinToMemory: true})}, OpenConfig{PinToMemory: true, WarmUp: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Synchronizer{}
			for _, o := range c.opts {
				o(s)
			}
			s.applyWarmUpDefault()
			if got := s.openConfig; got.WarmUp != c.want.WarmUp || got.Warm != c.want.Warm || got.PinToMemory != c.want.PinToMemory {
				t.Fatalf("openConfig = %+v, want %+v", got, c.want)
			}
		})
	}
}

// NewSynchronizer applies the default to the config it maps files with, and the files it installs still read correctly.
func TestNewSynchronizerWarmsByDefault(t *testing.T) {
	srv := newSigningServer(t)
	srv.blob.Store(readBlob(t, kvBlob(t, 50)))
	for _, opt := range []struct {
		name string
		opts []Option
		want bool
	}{{"default", nil, true}, {"opt out", []Option{WithWarmUp(false)}, false}} {
		t.Run(opt.name, func(t *testing.T) {
			opts := append([]Option{WithBaseURL(srv.srv.URL), WithCacheDir(t.TempDir()), WithSyncInterval(0), WithTelemetryDisabled()}, opt.opts...)
			s, err := NewSynchronizer(context.Background(), "ds-1", opts...)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if s.openConfig.WarmUp != opt.want {
				t.Fatalf("WarmUp=%v, want %v", s.openConfig.WarmUp, opt.want)
			}
			if v, err := s.Get([]byte("k7")); err != nil || string(v) != "v7" {
				t.Fatalf("lookup after a warmed install: %q %v", v, err)
			}
		})
	}
}
