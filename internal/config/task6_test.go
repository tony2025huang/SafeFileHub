package config

import (
	"testing"
	"time"
)

func TestRequestTimeoutIsOptionalAndSocketTimeoutsAreIndependent(t *testing.T) {
	cfg := Default()
	if cfg.RequestTimeout != 0 { t.Fatalf("RequestTimeout = %s, want 0", cfg.RequestTimeout) }
	if cfg.ReadHeaderTimeout <= 0 || cfg.IdleTimeout <= 0 { t.Fatalf("default socket timeouts must be positive") }
	if cfg.ReadTimeout != 0 || cfg.WriteTimeout != 0 { t.Fatalf("streaming read/write must default unlimited") }
	cfg.RequestTimeout = -time.Second
	if err := cfg.Validate(); err == nil { t.Fatal("negative RequestTimeout accepted") }
}
