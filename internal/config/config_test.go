package config

import (
	"testing"
	"time"
)

func TestDefaultConfiguration(t *testing.T) {
	cfg := Default()

	if got, want := cfg.StorageRoot, "data"; got != want {
		t.Fatalf("StorageRoot = %q, want %q", got, want)
	}
	if got, want := cfg.SQLitePath, "data/safefilehub.db"; got != want {
		t.Fatalf("SQLitePath = %q, want %q", got, want)
	}
	if got, want := cfg.ListenAddr, ":8080"; got != want {
		t.Fatalf("ListenAddr = %q, want %q", got, want)
	}
	if got, want := cfg.UploadConcurrency, 16; got != want {
		t.Fatalf("UploadConcurrency = %d, want %d", got, want)
	}
	if got, want := cfg.DownloadConcurrency, 16; got != want {
		t.Fatalf("DownloadConcurrency = %d, want %d", got, want)
	}
	if got, want := cfg.PerUserUploadConcurrency, 4; got != want {
		t.Fatalf("PerUserUploadConcurrency = %d, want %d", got, want)
	}
	if got, want := cfg.PerIPUploadConcurrency, 8; got != want {
		t.Fatalf("PerIPUploadConcurrency = %d, want %d", got, want)
	}
	if got, want := cfg.ChunkSize, int64(8<<20); got != want {
		t.Fatalf("ChunkSize = %d, want %d", got, want)
	}
	if got, want := cfg.UploadIdleTimeout, 30*time.Minute; got != want {
		t.Fatalf("UploadIdleTimeout = %s, want %s", got, want)
	}
	if got, want := cfg.UploadSessionTTL, 24*time.Hour; got != want {
		t.Fatalf("UploadSessionTTL = %s, want %s", got, want)
	}
	if !cfg.NamePolicy.RejectLeadingDot || !cfg.NamePolicy.RejectLeadingTilde || !cfg.NamePolicy.RejectLeadingDollar || !cfg.NamePolicy.RejectLeadingHash {
		t.Fatal("default name policy must reject configured special leading characters")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default configuration is invalid: %v", err)
	}
}

func TestDefaultRejectsZeroAndNegativeLimits(t *testing.T) {
	cases := []struct {
		name string
		set  func(*Config, int)
	}{
		{"upload concurrency", func(c *Config, v int) { c.UploadConcurrency = v }},
		{"download concurrency", func(c *Config, v int) { c.DownloadConcurrency = v }},
		{"per-user upload concurrency", func(c *Config, v int) { c.PerUserUploadConcurrency = v }},
		{"per-IP upload concurrency", func(c *Config, v int) { c.PerIPUploadConcurrency = v }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, value := range []int{0, -1} {
				cfg := Default()
				tc.set(&cfg, value)
				if err := cfg.Validate(); err == nil {
					t.Fatalf("Validate() accepted %d", value)
				}
			}
		})
	}
}
