package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
		{"GET", "/login"}, {"POST", "/login"}, {"POST", "/logout"}, {"GET", "/session"},
		{"GET", "/roots/1/files"}, {"POST", "/api/uploads"}, {"HEAD", "/api/uploads/x"}, {"PATCH", "/api/uploads/x"}, {"DELETE", "/api/uploads/x"}, {"POST", "/api/uploads/x/complete"},
		{"GET", "/api/files/1"}, {"POST", "/api/files"}, {"POST", "/api/roots/1/archives"}, {"GET", "/api/archives/x"}, {"DELETE", "/api/archives/x"}, {"GET", "/api/admin/audit"},
		{"GET", "/api/site-settings"},
		{"GET", "/api/admin/site-settings"}, {"PUT", "/api/admin/site-settings"}, {"POST", "/api/admin/site-settings/assets/favicon"}, {"DELETE", "/api/admin/site-settings/assets/favicon"},
	} {
		r := httptest.NewRequest(route.method, route.path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusNotFound || w.Code == http.StatusMethodNotAllowed {
			t.Errorf("%s %s is not registered: %d", route.method, route.path, w.Code)
		}
	}
	loginPage := httptest.NewRecorder()
	h.ServeHTTP(loginPage, httptest.NewRequest(http.MethodGet, "/login", nil))
	if loginPage.Code != http.StatusOK || loginPage.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("public production login page = %d %q", loginPage.Code, loginPage.Header().Get("Content-Type"))
	}
	if body := loginPage.Body.String(); body == "" || strings.Contains(body, `src="./app.js"`) || strings.Contains(body, `src="/app.js"`) {
		t.Fatalf("production login page is not self-contained: %q", body)
	}
	// These routes legitimately return 404 when no branding asset is configured;
	// a method mismatch proves their GET-only public contracts are installed.
	for _, path := range []string{"/assets/site/1", "/favicon.ico"} {
		r := httptest.NewRequest(http.MethodPut, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s route contract missing: %d", path, w.Code)
		}
	}
}

func TestNewProductionServerInitialAdminAccessesSiteSettingsWithDefaultConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StorageRoot = filepath.Join(dir, "data")
	cfg.SQLitePath = filepath.Join(dir, "safefilehub.db")
	if err := os.MkdirAll(cfg.StorageRoot, 0700); err != nil {
		t.Fatal(err)
	}
	repo, err := db.Open(ctx, cfg.SQLitePath)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	initial, err := repo.CreateUser(ctx, db.User{Username: "sfh-random-bootstrap-name", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetBootstrapAdmin(ctx, initial.ID); err != nil {
		t.Fatal(err)
	}
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
	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{InsecureCookie: true})
	defer sessions.Close()
	h, err := NewProductionServer(cfg, auth.NewService(repo), sessions, repo, permission.NewAuthorizer(repo, cfg.NamePolicy), store, manager, ProductionReadiness{DB: repo, ObjectStore: store, StoragePath: cfg.StorageRoot}, metrics.New(), limiter)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/admin/site-settings", nil)
	req.AddCookie(adminSession(t, sessions, initial.ID))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("initial admin site settings with default config = %d: %s", w.Code, w.Body.String())
	}
}
