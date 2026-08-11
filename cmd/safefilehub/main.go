// SafeFileHub serves files from configured logical storage roots.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/example/safefilehub/internal/archive"
	"github.com/example/safefilehub/internal/auth"
	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/httpapi"
	"github.com/example/safefilehub/internal/limits"
	"github.com/example/safefilehub/internal/metrics"
	appLog "github.com/example/safefilehub/internal/observability"
	"github.com/example/safefilehub/internal/permission"
	"github.com/example/safefilehub/internal/publishedrecovery"
	"github.com/example/safefilehub/internal/storage"
	"github.com/example/safefilehub/internal/upload"
)

type maintenanceOptions struct {
	recoverOnStart    bool
	recoverOnly       bool
	resetInitialAdmin bool
	dryRun            bool
	limit             int
	logPath           string
	logFormat         string
	logRetentionDays  int
	logMaxBytes       int64
	logBackups        int
	trustedProxyCIDRs cidrList
}

type cidrList []string

func (c *cidrList) String() string { return fmt.Sprint([]string(*c)) }
func (c *cidrList) Set(v string) error {
	if v == "" {
		return errors.New("trusted proxy CIDR is empty")
	}
	*c = append(*c, v)
	return nil
}

func parseMaintenanceOptions(args []string, cfg config.Config) (maintenanceOptions, error) {
	fs := flag.NewFlagSet("safefilehub", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	opts := maintenanceOptions{limit: cfg.UploadRecoveryLimit}
	fs.BoolVar(&opts.recoverOnStart, "recover-on-start", true, "run one bounded upload recovery pass before serving")
	fs.BoolVar(&opts.recoverOnly, "recover-only", false, "run upload recovery then exit")
	fs.BoolVar(&opts.resetInitialAdmin, "reset-initial-admin", false, "reset user id 1 credentials then exit")
	fs.BoolVar(&opts.dryRun, "recover-dry-run", false, "report upload recovery actions without changing files or metadata")
	fs.IntVar(&opts.limit, "recover-limit", opts.limit, "maximum staging files to inspect per recovery pass (1-64)")
	fs.StringVar(&opts.logPath, "log-path", cfg.LogPath, "optional application log file")
	fs.StringVar(&opts.logFormat, "log-format", cfg.LogFormat, "application log format: json or text")
	fs.IntVar(&opts.logRetentionDays, "log-retention-days", cfg.LogRetentionDays, "delete rotated logs older than this many days (0 disables age retention)")
	fs.Int64Var(&opts.logMaxBytes, "log-max-bytes", 100<<20, "rotate log file after this many bytes (0 disables size rotation)")
	fs.IntVar(&opts.logBackups, "log-backups", 10, "retain at most this many rotated logs (0 disables count retention)")
	fs.Var(&opts.trustedProxyCIDRs, "trusted-proxy-cidr", "trusted reverse proxy CIDR; repeatable")
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
	if opts.logFormat != "" && opts.logFormat != "json" && opts.logFormat != "text" {
		return maintenanceOptions{}, errors.New("-log-format must be json or text")
	}
	if opts.logRetentionDays < 0 || opts.logMaxBytes < 0 || opts.logBackups < 0 {
		return maintenanceOptions{}, errors.New("log retention, size and backups must not be negative")
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
	cfg.LogPath, cfg.LogFormat, cfg.LogRetentionDays = opts.logPath, opts.logFormat, opts.logRetentionDays
	if opts.trustedProxyCIDRs != nil {
		cfg.TrustedProxyCIDRs = []string(opts.trustedProxyCIDRs)
	}
	var rotating io.Closer
	var sinks []*appLog.Logger
	sinks = append(sinks, appLog.New(os.Stderr, appLog.Format(cfg.LogFormat)))
	if cfg.LogPath != "" {
		writer, err := appLog.NewRotatingWriter(cfg.LogPath, opts.logMaxBytes, cfg.LogRetentionDays, opts.logBackups)
		if err != nil {
			return err
		}
		rotating = writer
		sinks = append(sinks, appLog.New(writer, appLog.Format(cfg.LogFormat)))
		log.SetOutput(io.MultiWriter(os.Stderr, writer))
	}
	defer func() {
		if rotating != nil {
			_ = rotating.Close()
		}
	}()
	if errors.Is(lifecycle.Err(), context.Canceled) {
		log.Printf("SafeFileHub upload recovery cancelled: %v", lifecycle.Err())
		return nil
	}

	repo, err := db.Open(lifecycle, cfg.SQLitePath)
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()

	if opts.resetInitialAdmin {
		credentials, err := resetInitialAdmin(lifecycle, repo)
		if err != nil {
			return err
		}
		log.Printf("SafeFileHub initial administrator reset: username=%s password=%s", credentials.Username, credentials.Password)
		return nil
	}
	credentials, err := bootstrapInitialAdmin(lifecycle, repo)
	if err != nil {
		return err
	}
	if credentials.Created {
		log.Printf("SafeFileHub initial administrator created: username=%s password=%s", credentials.Username, credentials.Password)
	}

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

	observability := metrics.New()
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
			if !opts.dryRun {
				for range report.Cancelled + report.Orphans {
					observability.IncCleanup()
				}
			}
			log.Printf("SafeFileHub upload recovery: checked=%d kept=%d cleaned=%d orphans=%d dry_run=%t limit=%d", report.Checked, report.Kept, report.Cancelled, report.Orphans, opts.dryRun, opts.limit)
		}
	}
	if opts.recoverOnStart {
		ctx, cancel := context.WithTimeout(lifecycle, 10*time.Second)
		report, recoveryErr := publishedrecovery.Recover(ctx, repo, store, opts.limit)
		cancel()
		if recoveryErr != nil {
			log.Printf("SafeFileHub published recovery failed: %v", recoveryErr)
		} else {
			log.Printf("SafeFileHub published recovery: cleanup_checked=%d cleanup_completed=%d tombstones_checked=%d tombstones_finalized=%d limit=%d", report.CleanupChecked, report.CleanupCompleted, report.TombstonesChecked, report.TombstonesFinalized, opts.limit)
		}
	}
	if opts.recoverOnly {
		return nil
	}

	archiveManager, err := archive.New(archive.Options{Workers: 2, MaxFiles: 1000, MaxBytes: 1 << 30, TTL: time.Hour, TempDir: cfg.StorageRoot + "/archives"}, httpapi.ObjectArchiveSource{Store: store})
	if err != nil {
		return err
	}
	defer archiveManager.Close()

	h, err := httpapi.NewProductionServer(cfg, auth.NewService(repo), sessions, repo, permission.NewAuthorizer(repo, cfg.NamePolicy), store, archiveManager, httpapi.ProductionReadiness{DB: repo, ObjectStore: store, StoragePath: cfg.StorageRoot}, observability, limiter)
	if err != nil {
		return err
	}
	h = httpapi.WithApplicationLogger(appLog.NewMulti(sinks...), h)

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
