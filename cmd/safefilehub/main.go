// SafeFileHub serves files from configured logical storage roots.
package main

import (
	"context"
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
)

func main() {
	cfg := config.Default()
	lifecycle, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	repo, err := db.Open(lifecycle, cfg.SQLitePath)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = repo.Close() }()

	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{LifecycleContext: lifecycle})
	defer sessions.Close()
	limiter, err := limits.NewUploadLimiter(cfg.UploadConcurrency, cfg.PerUserUploadConcurrency, cfg.PerIPUploadConcurrency)
	if err != nil {
		log.Fatal(err)
	}
	store, err := storage.NewObjectStore(cfg.StorageRoot)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	h, err := httpapi.NewServerWithUploads(cfg, auth.NewService(repo), sessions, repo, permission.NewAuthorizer(repo, cfg.NamePolicy), store, limiter)
	if err != nil {
		log.Fatal(err)
	}

	server := httpapi.ServerTimeouts(cfg, h)
	go func() {
		<-lifecycle.Done()
		_ = server.Close()
	}()
	log.Printf("SafeFileHub listening on %s", cfg.ListenAddr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("SafeFileHub server: %v", err)
	}
}
