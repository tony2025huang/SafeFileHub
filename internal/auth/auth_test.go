package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/safefilehub/internal/auth"
	"github.com/example/safefilehub/internal/db"
)

func TestPasswordHashAndAuthenticate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := testRepository(t)

	hash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if hash == "correct horse battery staple" || !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("password hash = %q, want Argon2id encoded non-plaintext hash", hash)
	}
	user, err := repo.CreateUser(ctx, db.User{Username: "alice", PasswordHash: hash})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	service := auth.NewService(repo)
	got, err := service.Authenticate(ctx, "alice", "correct horse battery staple")
	if err != nil || got.ID != user.ID {
		t.Fatalf("authenticate valid password = %#v, %v", got, err)
	}
	if _, err := service.Authenticate(ctx, "alice", "wrong password"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("wrong password error = %v, want ErrInvalidCredentials", err)
	}
}

func TestAuthenticateRejectsDisabledUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := testRepository(t)
	hash, err := auth.HashPassword("password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateUser(ctx, db.User{Username: "disabled", PasswordHash: hash, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.NewService(repo).Authenticate(ctx, "disabled", "password"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("disabled user error = %v, want ErrInvalidCredentials", err)
	}
}

func TestSessionCookieIsSecureAsConfiguredAndExpires(t *testing.T) {
	t.Parallel()
	store := auth.NewMemorySessionStore()
	now := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)
	manager := auth.NewSessionManager(store, auth.SessionConfig{
		CookieName: "safefilehub_session", TTL: time.Hour, Secure: true, Now: func() time.Time { return now },
	})
	id, err := manager.Create(context.Background(), 42)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if len(id) < 32 {
		t.Fatalf("session id too short: %q", id)
	}

	rr := httptest.NewRecorder()
	manager.SetCookie(rr, id)
	cookie := rr.Result().Cookies()[0]
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Value != id || !cookie.Expires.Equal(now.Add(time.Hour)) {
		t.Fatalf("session cookie = %#v", cookie)
	}
	if strings.Contains(cookie.Value, "42") {
		t.Fatalf("cookie leaks user data: %q", cookie.Value)
	}
	if _, err := manager.UserID(context.Background(), id); err != nil {
		t.Fatalf("read fresh session: %v", err)
	}

	now = now.Add(time.Hour)
	if _, err := manager.UserID(context.Background(), id); !errors.Is(err, auth.ErrSessionExpired) {
		t.Fatalf("expired session error = %v, want ErrSessionExpired", err)
	}
}

func TestSessionCookieCanDisableSecureFlag(t *testing.T) {
	t.Parallel()
	manager := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{TTL: time.Hour, Secure: false})
	rr := httptest.NewRecorder()
	manager.SetCookie(rr, "random-server-side-id")
	if cookie := rr.Result().Cookies()[0]; cookie.Secure {
		t.Fatal("cookie Secure = true, want false")
	}
}

func testRepository(t *testing.T) *db.Repository {
	t.Helper()
	repo, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "auth.sqlite"))
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func TestSessionCookieUsesConfiguredSameSiteAndExpiresWithTTL(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)
	manager := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{
		TTL:      90 * time.Minute,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		Now:      func() time.Time { return now },
	})

	rr := httptest.NewRecorder()
	manager.SetCookie(rr, "opaque-server-session-id")
	cookie := rr.Result().Cookies()[0]
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie SameSite = %v, want %v", cookie.SameSite, http.SameSiteStrictMode)
	}
	if cookie.MaxAge != int((90*time.Minute).Seconds()) || !cookie.Expires.Equal(now.Add(90*time.Minute)) {
		t.Fatalf("cookie expiry = MaxAge %d, Expires %s; want %d, %s", cookie.MaxAge, cookie.Expires, int((90 * time.Minute).Seconds()), now.Add(90*time.Minute))
	}
}
