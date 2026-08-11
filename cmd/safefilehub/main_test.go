package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
)

func TestParseMaintenanceOptionsDefaultsToEnabledBoundedRecovery(t *testing.T) {
	opts, err := parseMaintenanceOptions(nil, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if !opts.recoverOnStart || opts.recoverOnly || opts.dryRun || opts.limit != 64 {
		t.Fatalf("defaults = %#v", opts)
	}
}

func TestParseMaintenanceOptionsSupportsRecoverOnlyDryRun(t *testing.T) {
	opts, err := parseMaintenanceOptions([]string{"-recover-only", "-recover-dry-run", "-recover-limit=7"}, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if !opts.recoverOnStart || !opts.recoverOnly || !opts.dryRun || opts.limit != 7 {
		t.Fatalf("options = %#v", opts)
	}
}

func TestParseMaintenanceOptionsRejectsUnboundedOrConflictingRequests(t *testing.T) {
	for _, args := range [][]string{
		{"-recover-limit=0"},
		{"-recover-limit=65"},
		{"-recover-only", "-recover-on-start=false"},
	} {
		if _, err := parseMaintenanceOptions(args, config.Default()); err == nil {
			t.Fatalf("parseMaintenanceOptions(%q) succeeded", args)
		}
	}
}

func TestRunRecoverOnlyReturnsErrorForRecoveryFailure(t *testing.T) {
	cfg := testConfig(t)
	// The storage root exists but staging does not, so Recover fails after all
	// resources have been initialized.
	err := runWithLifecycle(context.Background(), []string{"-recover-only"}, cfg)
	if err == nil {
		t.Fatal("runWithLifecycle() succeeded after recovery failure")
	}
	if got := exitCode(err); got == 0 {
		t.Fatal("exitCode(recovery error) = 0, want non-zero")
	}
	if _, err := os.Stat(cfg.SQLitePath); err != nil {
		t.Fatalf("database was not created: %v", err)
	}
	repo, err := db.Open(context.Background(), cfg.SQLitePath)
	if err != nil {
		t.Fatalf("database remains unavailable after recovery failure: %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRunRecoverOnlySucceedsAfterRecovery(t *testing.T) {
	cfg := testConfig(t)
	if err := os.Mkdir(filepath.Join(cfg.StorageRoot, "staging"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StorageRoot, "staging", "ignore.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runWithLifecycle(context.Background(), []string{"-recover-only", "-recover-dry-run"}, cfg); err != nil {
		t.Fatalf("runWithLifecycle() error = %v", err)
	}
}

func TestRunRecoverOnlyTreatsContextCancellationAsGraceful(t *testing.T) {
	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runWithLifecycle(ctx, []string{"-recover-only"}, cfg); err != nil {
		t.Fatalf("runWithLifecycle() error = %v, want nil for cancellation", err)
	}
}

func TestExitCode(t *testing.T) {
	if got := exitCode(nil); got != 0 {
		t.Fatalf("exitCode(nil) = %d, want 0", got)
	}
	if got := exitCode(errors.New("recovery failed")); got == 0 {
		t.Fatal("exitCode(error) = 0, want non-zero")
	}
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default()
	cfg.StorageRoot = filepath.Join(root, "data")
	cfg.SQLitePath = filepath.Join(cfg.StorageRoot, "safefilehub.db")
	if err := os.Mkdir(cfg.StorageRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	return cfg
}
