package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example/safefilehub/internal/archive"
	"github.com/example/safefilehub/internal/auth"
	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/limits"
	"github.com/example/safefilehub/internal/metrics"
	"github.com/example/safefilehub/internal/permission"
	"github.com/example/safefilehub/internal/storage"
)

// TestNewProductionServerRouteContract is a smoke contract for the one
// production composition root: every externally supported route is registered
// on one mux and shares its process dependencies.
func TestNewProductionServerRouteContract(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StorageRoot = filepath.Join(dir, "data")
	cfg.SQLitePath = filepath.Join(dir, "safefilehub.db")
	if err := os.MkdirAll(cfg.StorageRoot, 0700); err != nil {
		t.Fatal(err)
	}
	repo, err := db.Open(context.Background(), cfg.SQLitePath)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	store, err := storage.NewObjectStore(cfg.StorageRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := archive.New(archive.Options{Workers: 1, MaxFiles: 10, MaxBytes: 1 << 20, TTL: time.Minute, TempDir: filepath.Join(dir, "archives")}, ObjectArchiveSource{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	limiter, err := limits.NewUploadLimiter(1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewProductionServer(cfg, auth.NewService(repo), auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{}), repo, permission.NewAuthorizer(repo, cfg.NamePolicy), store, manager, ProductionReadiness{DB: repo, ObjectStore: store, StoragePath: cfg.StorageRoot}, metrics.New(), limiter)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ method, path string }{
		{"GET", "/healthz"}, {"GET", "/readyz"}, {"GET", "/metrics"}, {"GET", "/"},
		{"POST", "/login"}, {"POST", "/logout"}, {"GET", "/session"},
		{"GET", "/roots/1/files"}, {"POST", "/api/uploads"}, {"HEAD", "/api/uploads/x"}, {"PATCH", "/api/uploads/x"}, {"DELETE", "/api/uploads/x"}, {"POST", "/api/uploads/x/complete"},
		{"GET", "/api/files/1"}, {"POST", "/api/files"}, {"POST", "/api/roots/1/archives"}, {"GET", "/api/archives/x"}, {"DELETE", "/api/archives/x"}, {"GET", "/api/admin/audit"},
	} {
		r := httptest.NewRequest(route.method, route.path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusNotFound || w.Code == http.StatusMethodNotAllowed {
			t.Errorf("%s %s is not registered: %d", route.method, route.path, w.Code)
		}
	}
}
