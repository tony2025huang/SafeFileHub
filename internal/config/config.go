// Package config defines SafeFileHub's deterministic runtime configuration.
package config

import (
	"fmt"
	"time"
)

type NamePolicy struct {
	RejectLeadingDot    bool
	RejectLeadingTilde  bool
	RejectLeadingDollar bool
	RejectLeadingHash   bool
}

type Config struct {
	StorageRoot              string
	SQLitePath               string
	ListenAddr               string
	UploadConcurrency        int
	DownloadConcurrency      int
	PerUserUploadConcurrency int
	PerIPUploadConcurrency   int
	ChunkSize                int64
	UploadIdleTimeout        time.Duration
	UploadSessionTTL         time.Duration
	// UploadRecoveryLimit bounds one startup or explicit maintenance scan.
	// It must stay within the hard cap so recovery never becomes an unbounded job.
	UploadRecoveryLimit int
	MaxRequestBodyBytes int64
	// RequestTimeout is an optional total handler deadline. Zero permits
	// streaming requests to continue without a wall-clock handler deadline.
	RequestTimeout    time.Duration
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	// ShutdownTimeout bounds graceful HTTP shutdown after lifecycle cancellation.
	ShutdownTimeout time.Duration
	// Retained for backwards configuration compatibility; no longer used as a
	// handler or socket timeout.
	RequestIdleTimeout time.Duration
	NamePolicy         NamePolicy
}

func Default() Config {
	return Config{StorageRoot: "data", SQLitePath: "data/safefilehub.db", ListenAddr: ":8080", UploadConcurrency: 16, DownloadConcurrency: 16, PerUserUploadConcurrency: 4, PerIPUploadConcurrency: 8, ChunkSize: 8 << 20, UploadIdleTimeout: 30 * time.Minute, UploadSessionTTL: 24 * time.Hour, UploadRecoveryLimit: 64, MaxRequestBodyBytes: 64 << 20, RequestTimeout: 0, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 0, WriteTimeout: 0, IdleTimeout: 30 * time.Minute, ShutdownTimeout: 15 * time.Second, RequestIdleTimeout: 30 * time.Minute, NamePolicy: NamePolicy{RejectLeadingDot: true, RejectLeadingTilde: true, RejectLeadingDollar: true, RejectLeadingHash: true}}
}

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
	if c.UploadRecoveryLimit <= 0 || c.UploadRecoveryLimit > 64 {
		return fmt.Errorf("upload recovery limit must be between 1 and 64")
	}
	if c.MaxRequestBodyBytes <= 0 {
		return fmt.Errorf("max request body bytes must be positive")
	}
	if c.RequestTimeout < 0 {
		return fmt.Errorf("request timeout must not be negative")
	}
	if c.ReadHeaderTimeout <= 0 {
		return fmt.Errorf("read header timeout must be positive")
	}
	if c.ReadTimeout < 0 || c.WriteTimeout < 0 {
		return fmt.Errorf("read and write timeouts must not be negative")
	}
	if c.IdleTimeout <= 0 {
		return fmt.Errorf("idle timeout must be positive")
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("shutdown timeout must be positive")
	}
	if c.RequestIdleTimeout <= 0 {
		return fmt.Errorf("request idle timeout must be positive")
	}
	return nil
}
