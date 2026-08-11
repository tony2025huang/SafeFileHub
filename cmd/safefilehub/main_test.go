package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

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

type blockingMD5Worker struct{ stopped chan struct{} }

func (w blockingMD5Worker) Run(ctx context.Context, _ time.Duration) error {
	<-ctx.Done()
	close(w.stopped)
	return nil
}

func TestMD5WorkerLifecycleStopsAndWaits(t *testing.T) {
	worker := blockingMD5Worker{stopped: make(chan struct{})}
	stop := startMD5Worker(context.Background(), worker, time.Hour)
	stop()
	stop() // Shutdown may be reached through more than one cleanup path.
	select {
	case <-worker.stopped:
	default:
		t.Fatal("stop returned before MD5 worker exited")
	}
}

type lifecycleServer struct {
	shutdown chan struct{}
}

func (s lifecycleServer) Shutdown(context.Context) error {
	close(s.shutdown)
	return nil
}

func TestServeWithLifecycleGracefullyShutsDownAndWaits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := lifecycleServer{shutdown: make(chan struct{})}
	serveExited := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- serveWithLifecycle(ctx, time.Second, server, func() error {
			<-server.shutdown
			close(serveExited)
			return http.ErrServerClosed
		})
	}()

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serveWithLifecycle() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serveWithLifecycle did not complete graceful shutdown")
	}
	select {
	case <-serveExited:
	default:
		t.Fatal("serveWithLifecycle returned before serving goroutine exited")
	}
}

func TestServeWithLifecycleReleasesWatcherAfterStartupFailure(t *testing.T) {
	server := lifecycleServer{shutdown: make(chan struct{})}
	want := errors.New("listen failed")
	if err := serveWithLifecycle(context.Background(), time.Second, server, func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("serveWithLifecycle() error = %v, want wrapped %v", err, want)
	}
	select {
	case <-server.shutdown:
		t.Fatal("shutdown called after startup failure")
	default:
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
