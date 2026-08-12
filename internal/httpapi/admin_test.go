package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/example/safefilehub/internal/auth"
	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
)

func TestAdminUserAndPermissionManagement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, err := db.Open(ctx, filepath.Join(t.TempDir(), "admin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	adminHash, err := auth.HashPassword("admin-password")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := repo.CreateUser(ctx, db.User{Username: "admin", PasswordHash: adminHash})
	if err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateStorageRoot(ctx, db.StorageRoot{Name: "primary", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AdminUsernames = []string{"admin"}
	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{TTL: time.Hour, InsecureCookie: true})
	defer sessions.Close()
	h, err := NewServerWithAdmin(cfg, auth.NewService(repo), sessions, repo)
	if err != nil {
		t.Fatal(err)
	}
	adminCookie := adminSession(t, sessions, admin.ID)

	created := adminJSON(t, h, adminCookie, http.MethodPost, "/api/admin/users", `{"username":"bob","password":"bob-secret"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create user = %d: %s", created.Code, created.Body.String())
	}
	if strings.Contains(created.Body.String(), "bob-secret") || strings.Contains(created.Body.String(), "password_hash") {
		t.Fatalf("create response leaked password: %s", created.Body.String())
	}
	var user struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		Disabled bool   `json:"disabled"`
	}
	if err := json.NewDecoder(created.Body).Decode(&user); err != nil {
		t.Fatal(err)
	}
	if user.ID <= 0 || user.Username != "bob" || user.Disabled {
		t.Fatalf("created user = %#v", user)
	}

	permission := adminJSON(t, h, adminCookie, http.MethodPut, "/api/admin/users/"+itoa(user.ID)+"/permissions", `{"root_id":`+itoa(root.ID)+`,"path_prefix":"/docs","action":"read","allow":true}`)
	if permission.Code != http.StatusNoContent {
		t.Fatalf("set permission = %d: %s", permission.Code, permission.Body.String())
	}
	invalid := adminJSON(t, h, adminCookie, http.MethodPut, "/api/admin/users/"+itoa(user.ID)+"/permissions", `{"root_id":`+itoa(root.ID)+`,"path_prefix":"/%2e%2e/escape","action":"read","allow":true}`)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid prefix = %d, want 400", invalid.Code)
	}

	userCookie := adminSession(t, sessions, user.ID)
	reset := adminJSON(t, h, adminCookie, http.MethodPut, "/api/admin/users/"+itoa(user.ID)+"/password", `{"password":"new-secret"}`)
	if reset.Code != http.StatusNoContent || strings.Contains(reset.Body.String(), "new-secret") {
		t.Fatalf("reset = %d: %s", reset.Code, reset.Body.String())
	}
	disabled := adminJSON(t, h, adminCookie, http.MethodPut, "/api/admin/users/"+itoa(user.ID)+"/disabled", `{"disabled":true}`)
	if disabled.Code != http.StatusNoContent {
		t.Fatalf("disable = %d: %s", disabled.Code, disabled.Body.String())
	}
	status := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/session", nil)
	req.AddCookie(userCookie)
	h.ServeHTTP(status, req)
	if status.Code != http.StatusUnauthorized {
		t.Fatalf("disabled session = %d, want 401", status.Code)
	}

	for _, event := range mustAudit(t, repo, admin.ID) {
		if strings.Contains(event.Detail, "secret") || strings.Contains(event.Detail, "password") {
			t.Fatalf("audit leaked secret: %#v", event)
		}
	}
}

func TestBootstrapAdminAuthorizesWithEmptyConfiguredUsernames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, err := db.Open(ctx, filepath.Join(t.TempDir(), "admin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ordinary, err := repo.CreateUser(ctx, db.User{Username: "ordinary-first-user", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if ordinary.ID != 1 {
		t.Fatalf("ordinary user ID = %d, want 1", ordinary.ID)
	}
	initial, err := repo.CreateUser(ctx, db.User{Username: "sfh-random-bootstrap-name", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetBootstrapAdmin(ctx, initial.ID); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default() // Production defaults deliberately do not need a plaintext username.
	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{InsecureCookie: true})
	defer sessions.Close()
	h, err := NewServerWithAdmin(cfg, auth.NewService(repo), sessions, repo)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()

	created := adminHTTPJSON(t, server, adminSession(t, sessions, initial.ID), `{"username":"managed","password":"password"}`)
	if created.StatusCode != http.StatusCreated {
		created.Body.Close()
		t.Fatalf("bootstrap admin with empty configured usernames = %d, want 201", created.StatusCode)
	}
	created.Body.Close()

	forbidden := adminHTTPJSON(t, server, adminSession(t, sessions, ordinary.ID), `{"username":"not-allowed","password":"password"}`)
	defer forbidden.Body.Close()
	if forbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("non-bootstrap user with empty configured usernames = %d, want 403", forbidden.StatusCode)
	}
}

func adminHTTPJSON(t *testing.T, server *httptest.Server, cookie *http.Cookie, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/admin/users", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAdminEndpointsRejectNonAdmins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, err := db.Open(ctx, filepath.Join(t.TempDir(), "admin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	hash, _ := auth.HashPassword("password")
	if _, err := repo.CreateUser(ctx, db.User{Username: "initial", PasswordHash: hash}); err != nil {
		t.Fatal(err)
	}
	user, err := repo.CreateUser(ctx, db.User{Username: "member", PasswordHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AdminUsernames = []string{"admin"}
	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{InsecureCookie: true})
	defer sessions.Close()
	h, err := NewServerWithAdmin(cfg, auth.NewService(repo), sessions, repo)
	if err != nil {
		t.Fatal(err)
	}
	r := adminJSON(t, h, adminSession(t, sessions, user.ID), http.MethodPost, "/api/admin/users", `{"username":"nope","password":"password"}`)
	if r.Code != http.StatusForbidden {
		t.Fatalf("non-admin = %d, want 403", r.Code)
	}
}

func adminSession(t *testing.T, sessions *auth.SessionManager, id int64) *http.Cookie {
	t.Helper()
	token, err := sessions.Create(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: sessions.CookieName(), Value: token}
}
func adminJSON(t *testing.T, h http.Handler, cookie *http.Cookie, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	return rr
}
func mustAudit(t *testing.T, repo *db.Repository, userID int64) []db.AuditEvent {
	t.Helper()
	values, err := repo.AuditEventsForUser(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) < 4 {
		t.Fatalf("audit event count = %d, want >= 4", len(values))
	}
	return values
}
func itoa(v int64) string { return strconv.FormatInt(v, 10) }
