package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/example/safefilehub/internal/auth"
	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
)

func TestAuthenticationLoginLogoutAndProtectedEndpoint(t *testing.T) {
	passwordHash, err := auth.HashPassword("correct password")
	if err != nil {
		t.Fatal(err)
	}
	users := fakeUsers{user: db.User{ID: 7, Username: "alice", PasswordHash: passwordHash}}
	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{TTL: time.Hour})
	defer sessions.Close()
	h, err := NewServerWithAuth(config.Default(), auth.NewService(users), sessions)
	if err != nil {
		t.Fatalf("NewServerWithAuth: %v", err)
	}

	unauthorized := httptest.NewRecorder()
	h.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/session", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("GET /session without cookie = %d, want 401", unauthorized.Code)
	}

	login := httptest.NewRecorder()
	h.ServeHTTP(login, httptest.NewRequest(http.MethodPost, "/login", bytes.NewBufferString(`{"username":"alice","password":"correct password"}`)))
	if login.Code != http.StatusNoContent {
		t.Fatalf("POST /login = %d, body %q", login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.Value == "" {
		t.Fatalf("login cookie = %#v", cookie)
	}

	protected := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/session", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(protected, req)
	if protected.Code != http.StatusOK || protected.Body.String() != "authenticated\n" {
		t.Fatalf("GET /session = %d, %q", protected.Code, protected.Body.String())
	}

	logout := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(logout, req)
	if logout.Code != http.StatusNoContent {
		t.Fatalf("POST /logout = %d", logout.Code)
	}
	cleared := logout.Result().Cookies()[0]
	if cleared.MaxAge != -1 || cleared.Value != "" {
		t.Fatalf("logout cookie = %#v", cleared)
	}

	revoked := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/session", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(revoked, req)
	if revoked.Code != http.StatusUnauthorized {
		t.Fatalf("GET /session after logout = %d, want 401", revoked.Code)
	}
}

type rejectingAuthenticator struct{}

func (rejectingAuthenticator) Authenticate(context.Context, string, string) (db.User, error) {
	return db.User{}, auth.ErrInvalidCredentials
}

type fakeUsers struct{ user db.User }

func (f fakeUsers) UserByUsername(_ context.Context, username string) (db.User, error) {
	if username == f.user.Username {
		return f.user, nil
	}
	return db.User{}, db.ErrNotFound
}

func TestHealthzReturnsSmallSuccessfulResponseWithoutStorageScan(t *testing.T) {
	cfg := config.Default()
	cfg.StorageRoot = "/path/that/must/not/be-scanned"

	h, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if got, want := recorder.Code, http.StatusOK; got != want {
		t.Fatalf("GET /healthz status = %d, want %d", got, want)
	}
	if body := recorder.Body.String(); body != "ok\n" {
		t.Fatalf("GET /healthz body = %q, want %q", body, "ok\n")
	}
	if size := recorder.Body.Len(); size > 64 {
		t.Fatalf("GET /healthz body is too large: %d bytes", size)
	}
}

func TestProtectedEndpointRejectsInvalidSessionPrincipal(t *testing.T) {
	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{TTL: time.Hour})
	defer sessions.Close()
	h, err := NewServerWithAuth(config.Default(), rejectingAuthenticator{}, sessions)
	if err != nil {
		t.Fatalf("NewServerWithAuth: %v", err)
	}
	id, err := sessions.Create(context.Background(), 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/session", nil)
	req.AddCookie(&http.Cookie{Name: sessions.CookieName(), Value: id})
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("GET /session with invalid principal = %d, want 403", recorder.Code)
	}
}
