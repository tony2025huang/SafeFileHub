package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/example/safefilehub/internal/archive"
	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/limits"
	"github.com/example/safefilehub/internal/metrics"
	"github.com/example/safefilehub/internal/siteassets"
	"github.com/example/safefilehub/internal/storage"
	"github.com/example/safefilehub/internal/upload"
)

// ProductionReadiness performs constant-cost dependency checks. It never
// enumerates object or staging directories.
type ProductionReadiness struct {
	DB          interface{ PingContext(context.Context) error }
	ObjectStore interface{ Check() error }
	StoragePath string
}

func (c ProductionReadiness) DatabaseCheck(r *http.Request) error {
	if c.DB == nil {
		return errors.New("database unavailable")
	}
	return c.DB.PingContext(r.Context())
}
func (c ProductionReadiness) StorageCheck(_ *http.Request) error {
	if c.ObjectStore == nil {
		return errors.New("storage unavailable")
	}
	return c.ObjectStore.Check()
}
func (c ProductionReadiness) DiskCheck(_ *http.Request) error {
	if c.StoragePath == "" {
		return errors.New("storage path unavailable")
	}
	_, err := os.Stat(c.StoragePath)
	return err
}
func (c ProductionReadiness) Database(r *http.Request) error { return c.DatabaseCheck(r) }
func (c ProductionReadiness) Storage(r *http.Request) error  { return c.StorageCheck(r) }
func (c ProductionReadiness) Disk(r *http.Request) error     { return c.DiskCheck(r) }

// NewProductionServer is SafeFileHub's only full production composition root.
// It builds one mux, reuses the caller-owned sessions, authorizer, metrics and
// limiters, and applies RequestLimits exactly once at the outer boundary.
func NewProductionServer(cfg config.Config, users authenticator, sessions userSessionRevoker, repo interface {
	uploadRepository
	downloadRepository
	archiveRepository
	siteSettingsRepository
	publishedRepository
}, authorizer interface {
	uploadAuthorizer
	downloadAuthorizer
	archiveAuthorizer
	fileAuthorizer
}, store *storage.ObjectStore, archives *archive.Manager, checks ReadinessChecks, observability *metrics.Metrics, uploadLimiter *limits.UploadLimiter) (http.Handler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if users == nil || sessions == nil || repo == nil || authorizer == nil || store == nil || archives == nil || checks == nil || observability == nil || uploadLimiter == nil {
		return nil, errors.New("production dependencies are required")
	}
	downloadLimiter, err := limits.NewDownloadLimiter(cfg.DownloadConcurrency)
	if err != nil {
		return nil, err
	}
	uploads := upload.New(repo, store, cfg.ChunkSize, cfg.UploadSessionTTL)
	siteAssetStore, err := siteassets.New(cfg.StorageRoot, siteassets.Limits{MaxBytes: 8 << 20, MaxPixels: 20_000_000})
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /readyz", readyz(checks))
	mux.HandleFunc("GET /", transferUI)
	mux.HandleFunc("GET /login", transferUI)
	mux.HandleFunc("POST /login", login(users, sessions))
	mux.HandleFunc("POST /logout", logout(sessions))
	mux.Handle("GET /session", requireSession(sessions, http.HandlerFunc(sessionStatus)))
	mux.Handle("GET /roots/{rootID}/files", requireSession(sessions, listFiles(repo, authorizer, cfg.NamePolicy)))
	mux.Handle("POST /api/uploads", requireSession(sessions, logTransferLifecycle("upload", http.HandlerFunc(createUpload(uploads, repo, authorizer, cfg)))))
	state := requireSession(sessions, http.HandlerFunc(uploadStateWithMetrics(uploads, authorizer, observability)))
	mux.Handle("HEAD /api/uploads/{id}", state)
	mux.Handle("DELETE /api/uploads/{id}", state)
	patch := UploadBodyLimits(cfg.UploadIdleTimeout, cfg.MaxRequestBodyBytes, http.HandlerFunc(patchUpload(uploads, authorizer)))
	mux.Handle("PATCH /api/uploads/{id}", requireSession(sessions, logTransferLifecycle("upload", limitUploadWithMetrics(uploadLimiter, time.Second, sessionUploadIdentity, observability, observeUpload(observability, patch)))))
	mux.Handle("POST /api/uploads/{id}/complete", requireSession(sessions, logTransferLifecycle("upload", http.HandlerFunc(completeUpload(uploads, authorizer)))))
	download := requireSession(sessions, logTransferLifecycle("download", limitDownloadWithMetrics(downloadLimiter, observability, observeDownload(observability, http.HandlerFunc(downloadFile(repo, authorizer, store))))))
	mux.Handle("GET /api/files/{fileID}", download)
	mux.Handle("POST /api/directories", requireSession(sessions, logTransferLifecycle("directory.create", http.HandlerFunc(createDirectory(repo, authorizer, cfg.NamePolicy)))))
	mux.Handle("POST /api/files", requireSession(sessions, logTransferLifecycle("file.create", http.HandlerFunc(createFile(repo, authorizer, cfg.NamePolicy, store, true)))))
	mux.Handle("PATCH /api/files/{fileID}", requireSession(sessions, logTransferLifecycle("file.rename", http.HandlerFunc(renameFile(repo, authorizer, cfg.NamePolicy)))))
	mux.Handle("DELETE /api/files/{fileID}", requireSession(sessions, logTransferLifecycle("file.delete", http.HandlerFunc(deleteFile(repo, authorizer, store)))))
	mux.Handle("DELETE /api/directories/{directoryID}", requireSession(sessions, logTransferLifecycle("directory.delete", http.HandlerFunc(deleteDirectory(repo, authorizer)))))
	mux.Handle("HEAD /api/files/{fileID}", download)
	// createArchive logs its archive lifecycle after it receives the durable job ID.
	mux.Handle("POST /api/roots/{rootID}/archives", requireSession(sessions, observeArchive(observability, http.HandlerFunc(createArchive(repo, authorizer, archives, cfg.NamePolicy)))))
	mux.Handle("GET /api/archives/{jobID}", requireSession(sessions, logTransferLifecycle("archive", observeArchive(observability, http.HandlerFunc(downloadArchive(archives))))))
	mux.Handle("DELETE /api/archives/{jobID}", requireSession(sessions, observeCancellation(observability, http.HandlerFunc(cancelArchive(archives)))))
	registerAdminRoutes(mux, cfg, sessions, repo)
	registerSiteSettingsRoutes(mux, cfg, sessions, repo, siteAssetStore)
	return observedHandler(cfg, mux, observability), nil
}

// ObjectArchiveSource safely adapts the object store to archive.Source.
type ObjectArchiveSource struct{ Store *storage.ObjectStore }

func (s ObjectArchiveSource) Open(_ context.Context, key string) (io.ReadCloser, error) {
	return s.Store.Open(key)
}
