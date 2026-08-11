// Package config defines SafeFileHub's deterministic runtime configuration.
package config

import (
	"fmt"
	"time"
)

// NamePolicy controls which special leading characters are disallowed in names.
type NamePolicy struct {
	RejectLeadingDot    bool
	RejectLeadingTilde  bool
	RejectLeadingDollar bool
	RejectLeadingHash   bool
}

// Config contains the runtime settings needed by the service.
type Config struct {
	StorageRoot string
	SQLitePath  string
	ListenAddr  string

	UploadConcurrency        int
	DownloadConcurrency      int
	PerUserUploadConcurrency int
	PerIPUploadConcurrency   int
	ChunkSize                int64

	UploadIdleTimeout time.Duration
	UploadSessionTTL  time.Duration
	// MaxRequestBodyBytes bounds HTTP request bodies before handlers read them.
	MaxRequestBodyBytes int64
	// RequestIdleTimeout bounds the lifetime of an individual request handler.
	RequestIdleTimeout time.Duration

	NamePolicy NamePolicy
}

// Default returns SafeFileHub's deterministic baseline configuration.
func Default() Config {
	return Config{
		StorageRoot:              "data",
		SQLitePath:               "data/safefilehub.db",
		ListenAddr:               ":8080",
		UploadConcurrency:        16,
		DownloadConcurrency:      16,
		PerUserUploadConcurrency: 4,
		PerIPUploadConcurrency:   8,
		ChunkSize:                8 << 20,
		UploadIdleTimeout:        30 * time.Minute,
		UploadSessionTTL:         24 * time.Hour,
		MaxRequestBodyBytes:      64 << 20,
		RequestIdleTimeout:       30 * time.Minute,
		NamePolicy: NamePolicy{
			RejectLeadingDot:    true,
			RejectLeadingTilde:  true,
			RejectLeadingDollar: true,
			RejectLeadingHash:   true,
		},
	}
}

// Validate rejects invalid configuration before the service starts.
func (c Config) Validate() error {
	if c.StorageRoot == "" {
		return fmt.Errorf("storage root must not be empty")
	}
	if c.SQLitePath == "" {
		return fmt.Errorf("SQLite path must not be empty")
	}
	if c.ListenAddr == "" {
		return fmt.Errorf("listen address must not be empty")
	}
	if c.UploadConcurrency <= 0 {
		return fmt.Errorf("upload concurrency must be positive")
	}
	if c.DownloadConcurrency <= 0 {
		return fmt.Errorf("download concurrency must be positive")
	}
	if c.PerUserUploadConcurrency <= 0 {
		return fmt.Errorf("per-user upload concurrency must be positive")
	}
	if c.PerIPUploadConcurrency <= 0 {
		return fmt.Errorf("per-IP upload concurrency must be positive")
	}
	if c.ChunkSize <= 0 {
		return fmt.Errorf("chunk size must be positive")
	}
	if c.UploadIdleTimeout <= 0 {
		return fmt.Errorf("upload idle timeout must be positive")
	}
	if c.UploadSessionTTL <= 0 {
		return fmt.Errorf("upload session TTL must be positive")
	}
	if c.MaxRequestBodyBytes <= 0 {
		return fmt.Errorf("max request body bytes must be positive")
	}
	if c.RequestIdleTimeout <= 0 {
		return fmt.Errorf("request idle timeout must be positive")
	}
	return nil
}
