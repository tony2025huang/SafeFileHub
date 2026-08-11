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
	"github.com/example/safefilehub/internal/limits"
)

func TestUploadRouteUsesAuthenticatedSessionAndLimiter(t *testing.T) {
	cfg := config.Default()
	cfg.UploadConcurrency, cfg.PerUserUploadConcurrency, cfg.PerIPUploadConcurrency = 1, 1, 1
	limiter, err := limits.NewUploadLimiter(1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{TTL: time.Hour})
	defer sessions.Close()
	id, err := sessions.Create(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewServerWithAuth(cfg, rejectingAuthenticator{}, sessions, limiter)
	if err != nil {
		t.Fatal(err)
	}

	lease, err := limiter.TryAcquire("7", "2001:db8::1")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	req := httptest.NewRequest(http.MethodPost, "/api/uploads", bytes.NewBufferString("ok"))
	req.RemoteAddr = "[2001:db8::1]:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.99")
	req.Header.Set("X-Real-IP", "198.51.100.98")
	req.AddCookie(&http.Cookie{Name: sessions.CookieName(), Value: id})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rr.Code)
	}
	if got := rr.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}

	lease.Release()
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("authenticated upload = %d, want 204", rr.Code)
	}
}

func TestUploadRouteRejectsMissingSessionAndOversizedBody(t *testing.T) {
	cfg := config.Default()
	cfg.MaxRequestBodyBytes = 3
	limiter, err := limits.NewUploadLimiter(2, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{TTL: time.Hour})
	defer sessions.Close()
	h, err := NewServerWithAuth(cfg, rejectingAuthenticator{}, sessions, limiter)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/uploads", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing session = %d, want 401", rr.Code)
	}

	id, err := sessions.Create(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/uploads", bytes.NewBufferString("four"))
	req.RemoteAddr = "192.0.2.9:4321"
	req.AddCookie(&http.Cookie{Name: sessions.CookieName(), Value: id})
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d, want 413", rr.Code)
	}
}

func TestUploadIdentityCanonicalizesRemoteAddrAndDoesNotTrustHeaders(t *testing.T) {
	for _, tc := range []struct{ remote, want string }{
		{"192.0.2.5:99", "192.0.2.5"},
		{"[2001:0db8:0:0::1]:99", "2001:db8::1"},
	} {
		t.Run(tc.remote, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.RemoteAddr = tc.remote
			req.Header.Set("X-Forwarded-For", "203.0.113.7")
			got, ok := clientIP(req)
			if !ok || got != tc.want {
				t.Fatalf("clientIP = %q, %v; want %q, true", got, ok, tc.want)
			}
		})
	}
}

func TestServerTimeoutsKeepStreamingRequestsUnlimitedUnlessRequestTimeoutSet(t *testing.T) {
	cfg := config.Default()
	cfg.RequestTimeout = 0
	cfg.ReadHeaderTimeout = 3 * time.Second
	cfg.ReadTimeout = 0
	cfg.WriteTimeout = 0
	cfg.IdleTimeout = 4 * time.Second
	s := ServerTimeouts(cfg, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if s.ReadHeaderTimeout != cfg.ReadHeaderTimeout || s.ReadTimeout != 0 || s.WriteTimeout != 0 || s.IdleTimeout != cfg.IdleTimeout {
		t.Fatalf("unexpected server timeouts: %#v", s)
	}
	observed := make(chan error, 1)
	h := RequestLimits(cfg, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { observed <- r.Context().Err() }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
	if err := <-observed; err != nil {
		t.Fatalf("unlimited request context error = %v", err)
	}
}
