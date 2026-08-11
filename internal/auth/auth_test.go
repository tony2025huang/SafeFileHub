package auth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

func TestVerifyPasswordRejectsUntrustedOrOversizedArgon2idParameters(t *testing.T) {
	hash, err := auth.HashPassword("password")
	if err != nil {
		t.Fatal(err)
	}
	for _, malicious := range []string{
		strings.Replace(hash, "m=65536", "m=262144", 1),
		strings.Replace(hash, "t=3", "t=4", 1),
		strings.Replace(hash, "p=1", "p=2", 1),
		strings.Replace(hash, "$argon2id$", "$argon2i$", 1),
		"$argon2id$v=19$m=65536,t=3,p=1$" + strings.Repeat("A", 4096) + "$" + strings.Repeat("A", 4096),
	} {
		if auth.VerifyPassword(malicious, "password") {
			t.Fatalf("VerifyPassword accepted untrusted hash %q", malicious)
		}
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

func TestSessionManagerGCIsBoundedAndPreservesFreshSessions(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)
	store := auth.NewMemorySessionStore()
	manager := auth.NewSessionManager(store, auth.SessionConfig{TTL: time.Hour, Now: func() time.Time { return now }})
	for i := 0; i < 65; i++ {
		if err := store.Create(context.Background(), auth.Session{ID: fmt.Sprintf("expired-%d", i), UserID: 1, ExpiresAt: now.Add(-time.Second)}); err != nil {
			t.Fatalf("seed expired session: %v", err)
		}
	}
	if err := store.Create(context.Background(), auth.Session{ID: "fresh", UserID: 2, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("seed fresh session: %v", err)
	}
	deleted, err := manager.GC(context.Background())
	if err != nil || deleted != 64 {
		t.Fatalf("GC = %d, %v; want 64, nil", deleted, err)
	}
	if _, err := store.Lookup(context.Background(), "fresh"); err != nil {
		t.Fatalf("fresh session was deleted: %v", err)
	}
}

type blockingSessionStore struct {
	auth.SessionStore
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (s *blockingSessionStore) DeleteExpired(ctx context.Context, now time.Time, limit int) (int, error) {
	s.calls.Add(1)
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-s.release
	return s.SessionStore.DeleteExpired(ctx, now, limit)
}

func TestSessionManagerCreateDoesNotSynchronouslyRunGC(t *testing.T) {
	store := &blockingSessionStore{SessionStore: auth.NewMemorySessionStore(), entered: make(chan struct{}, 1), release: make(chan struct{})}
	manager := auth.NewSessionManager(store, auth.SessionConfig{TTL: time.Hour})
	created := make(chan error, 1)
	go func() { _, err := manager.Create(context.Background(), 1); created <- err }()
	select {
	case err := <-created:
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Create blocked on session GC")
	}
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("Create did not schedule maintenance GC")
	}
	close(store.release)
}

func TestSessionManagerEventuallyDrainsUnaccessedExpiredBacklog(t *testing.T) {
	now := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)
	store := auth.NewMemorySessionStore()
	manager := auth.NewSessionManager(store, auth.SessionConfig{TTL: time.Hour, Now: func() time.Time { return now }})
	for i := 0; i < 130; i++ {
		if err := store.Create(context.Background(), auth.Session{ID: fmt.Sprintf("expired-%d", i), UserID: 1, ExpiresAt: now.Add(-time.Second)}); err != nil {
			t.Fatalf("seed expired session: %v", err)
		}
	}
	if _, err := manager.Create(context.Background(), 2); err != nil {
		t.Fatalf("Create: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		allDeleted := true
		for i := 0; i < 130; i++ {
			if _, err := store.Lookup(context.Background(), fmt.Sprintf("expired-%d", i)); !errors.Is(err, auth.ErrSessionNotFound) {
				allDeleted = false
				break
			}
		}
		if allDeleted {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("background GC did not drain expired backlog")
}

func TestSessionManagerConcurrentCreateLookupAndGCAreRaceSafe(t *testing.T) {
	store := auth.NewMemorySessionStore()
	manager := auth.NewSessionManager(store, auth.SessionConfig{TTL: time.Hour})
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				id, err := manager.Create(ctx, int64(j))
				if err == nil {
					_, _ = manager.UserID(ctx, id)
				}
				_, _ = manager.GC(ctx)
			}
		}()
	}
	wg.Wait()
}

func TestLogoutRevokesSessionAndClearsMatchingCookie(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)
	store := auth.NewMemorySessionStore()
	manager := auth.NewSessionManager(store, auth.SessionConfig{
		CookieName: "custom_session", TTL: time.Hour, Secure: true, SameSite: http.SameSiteStrictMode, Now: func() time.Time { return now },
	})
	id, err := manager.Create(context.Background(), 42)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := manager.Logout(context.Background(), id); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := manager.UserID(context.Background(), id); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("revoked session lookup = %v, want ErrSessionNotFound", err)
	}

	rr := httptest.NewRecorder()
	manager.ClearSessionCookie(rr)
	cookie := rr.Result().Cookies()[0]
	if cookie.Name != "custom_session" || cookie.Value != "" || cookie.Path != "/" || cookie.MaxAge != -1 || !cookie.Expires.Before(now) || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cleared cookie = %#v", cookie)
	}
}

func TestSessionManagerCloseCancelsBlockedGCAndPreventsRestart(t *testing.T) {
	store := &contextBlockingSessionStore{SessionStore: auth.NewMemorySessionStore(), entered: make(chan struct{}, 1), exited: make(chan struct{})}
	manager := auth.NewSessionManager(store, auth.SessionConfig{TTL: time.Hour, GCDeleteTimeout: time.Hour})
	if _, err := manager.Create(context.Background(), 1); err != nil {
		t.Fatalf("Create: %v", err)
	}
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("GC did not enter DeleteExpired")
	}
	manager.Close()
	manager.Close() // Close is safe to call repeatedly during layered shutdown.
	select {
	case <-store.exited:
	case <-time.After(time.Second):
		t.Fatal("Close did not wait for GC worker to exit")
	}
	calls := store.calls.Load()
	if _, err := manager.Create(context.Background(), 2); err != nil {
		t.Fatalf("Create after Close: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := store.calls.Load(); got != calls {
		t.Fatalf("GC calls after Close = %d, want %d", got, calls)
	}
}

type contextBlockingSessionStore struct {
	auth.SessionStore
	entered chan struct{}
	exited  chan struct{}
	calls   atomic.Int32
}

func (s *contextBlockingSessionStore) DeleteExpired(ctx context.Context, _ time.Time, _ int) (int, error) {
	s.calls.Add(1)
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	close(s.exited)
	return 0, ctx.Err()
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

func TestRevokeDeletesServerSideSession(t *testing.T) {
	store := auth.NewMemorySessionStore()
	manager := auth.NewSessionManager(store, auth.SessionConfig{TTL: time.Hour})
	id, err := manager.Create(context.Background(), 42)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := manager.Revoke(context.Background(), id); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := manager.UserID(context.Background(), id); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("revoked session lookup = %v, want ErrSessionNotFound", err)
	}
}

func TestSessionManagerGCTimeoutCancelsBlockingStore(t *testing.T) {
	store := &contextBlockingSessionStore{SessionStore: auth.NewMemorySessionStore(), entered: make(chan struct{}, 1), exited: make(chan struct{})}
	manager := auth.NewSessionManager(store, auth.SessionConfig{TTL: time.Hour, GCDeleteTimeout: 20 * time.Millisecond})
	defer manager.Close()
	if _, err := manager.Create(context.Background(), 1); err != nil {
		t.Fatalf("Create: %v", err)
	}
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("GC did not enter DeleteExpired")
	}
	select {
	case <-store.exited:
	case <-time.After(time.Second):
		t.Fatal("GC timeout did not release blocking store")
	}
}

func TestSessionManagerParentLifecycleCancellationStopsGC(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &contextBlockingSessionStore{SessionStore: auth.NewMemorySessionStore(), entered: make(chan struct{}, 1), exited: make(chan struct{})}
	manager := auth.NewSessionManager(store, auth.SessionConfig{TTL: time.Hour, LifecycleContext: parent, GCDeleteTimeout: time.Hour})
	defer manager.Close()
	if _, err := manager.Create(context.Background(), 1); err != nil {
		t.Fatalf("Create: %v", err)
	}
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("GC did not enter DeleteExpired")
	}
	cancel()
	select {
	case <-store.exited:
	case <-time.After(time.Second):
		t.Fatal("parent lifecycle cancellation did not stop GC")
	}
}
