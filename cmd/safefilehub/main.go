// SafeFileHub serves files from configured logical storage roots.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/example/safefilehub/internal/auth"
	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/httpapi"
	"github.com/example/safefilehub/internal/limits"
	"github.com/example/safefilehub/internal/permission"
	"github.com/example/safefilehub/internal/storage"
	"github.com/example/safefilehub/internal/upload"
)

type maintenanceOptions struct {
	recoverOnStart bool
	recoverOnly    bool
	dryRun         bool
	limit          int
}

func parseMaintenanceOptions(args []string, cfg config.Config) (maintenanceOptions, error) {
	fs := flag.NewFlagSet("safefilehub", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	opts := maintenanceOptions{limit: cfg.UploadRecoveryLimit}
	fs.BoolVar(&opts.recoverOnStart, "recover-on-start", true, "run one bounded upload recovery pass before serving")
	fs.BoolVar(&opts.recoverOnly, "recover-only", false, "run upload recovery then exit")
	fs.BoolVar(&opts.dryRun, "recover-dry-run", false, "report upload recovery actions without changing files or metadata")
	fs.IntVar(&opts.limit, "recover-limit", opts.limit, "maximum staging files to inspect per recovery pass (1-64)")
	if err := fs.Parse(args); err != nil {
		return maintenanceOptions{}, err
	}
	if fs.NArg() != 0 {
		return maintenanceOptions{}, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if opts.recoverOnly && !opts.recoverOnStart {
		return maintenanceOptions{}, errors.New("-recover-only requires -recover-on-start")
	}
	if opts.limit <= 0 || opts.limit > 64 {
		return maintenanceOptions{}, errors.New("-recover-limit must be between 1 and 64")
	}
	return opts, nil
}

func main() {
	err := run(os.Args[1:])
	if err != nil {
		log.Print(err)
	}
	os.Exit(exitCode(err))
}

func exitCode(err error) int {
	if err != nil {
		return 1
	}
	return 0
}

func run(args []string) error {
	lifecycle, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runWithLifecycle(lifecycle, args, config.Default())
}

func runWithLifecycle(lifecycle context.Context, args []string, cfg config.Config) error {
	opts, err := parseMaintenanceOptions(args, cfg)
	if err != nil {
		return err
	}
	if errors.Is(lifecycle.Err(), context.Canceled) {
		log.Printf("SafeFileHub upload recovery cancelled: %v", lifecycle.Err())
		return nil
	}

	repo, err := db.Open(lifecycle, cfg.SQLitePath)
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()

	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{LifecycleContext: lifecycle})
	defer sessions.Close()
	limiter, err := limits.NewUploadLimiter(cfg.UploadConcurrency, cfg.PerUserUploadConcurrency, cfg.PerIPUploadConcurrency)
	if err != nil {
		return err
	}
	store, err := storage.NewObjectStore(cfg.StorageRoot)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	if opts.recoverOnStart {
		recovery := upload.New(repo, store, cfg.ChunkSize, cfg.UploadSessionTTL)
		report, err := recovery.Recover(lifecycle, opts.limit, opts.dryRun)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				log.Printf("SafeFileHub upload recovery cancelled: %v", err)
				return nil
			}
			if opts.recoverOnly {
				return fmt.Errorf("SafeFileHub upload recovery: %w", err)
			}
			log.Printf("SafeFileHub upload recovery failed: %v", err)
		} else {
			log.Printf("SafeFileHub upload recovery: checked=%d kept=%d cleaned=%d orphans=%d dry_run=%t limit=%d", report.Checked, report.Kept, report.Cancelled, report.Orphans, opts.dryRun, opts.limit)
		}
	}
	if opts.recoverOnly {
		return nil
	}

	h, err := httpapi.NewServerWithUploads(cfg, auth.NewService(repo), sessions, repo, permission.NewAuthorizer(repo, cfg.NamePolicy), store, limiter)
	if err != nil {
		return err
	}

	server := httpapi.ServerTimeouts(cfg, h)
	go func() {
		<-lifecycle.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("SafeFileHub HTTP shutdown: %v", err)
		}
	}()
	log.Printf("SafeFileHub listening on %s", cfg.ListenAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("SafeFileHub server: %w", err)
	}
	return nil
}
