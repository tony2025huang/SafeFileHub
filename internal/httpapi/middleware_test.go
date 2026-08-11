package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/limits"
)

func TestLimitUploadReturns429AndRetryAfterWithoutCallingHandler(t *testing.T) {
	limiter, err := limits.NewUploadLimiter(1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := limiter.TryAcquire("alice", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	called := false
	h := LimitUpload(limiter, 7*time.Second, func(*http.Request) (string, string) { return "alice", "192.0.2.1" }, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/uploads", nil))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Retry-After"); got != "7" {
		t.Fatalf("Retry-After = %q, want 7", got)
	}
	if called {
		t.Fatal("limited request reached handler")
	}
}

func TestLimitUploadReleasesLeaseAfterSuccessErrorAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.Handler
		ctx     context.Context
	}{
		{"success", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }), context.Background()},
		{"error", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }), context.Background()},
		{"cancellation", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }), cancelledContext(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limiter, err := limits.NewUploadLimiter(1, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			h := LimitUpload(limiter, time.Second, func(*http.Request) (string, string) { return "alice", "192.0.2.1" }, tc.handler)
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/uploads", nil).WithContext(tc.ctx)
			if tc.name == "error" {
				func() { defer func() { _ = recover() }(); h.ServeHTTP(recorder, req) }()
			} else {
				h.ServeHTTP(recorder, req)
			}
			if lease, err := limiter.TryAcquire("alice", "192.0.2.1"); err != nil {
				t.Fatalf("lease not released: %v", err)
			} else {
				lease.Release()
			}
		})
	}
}

func TestRequestLimitsBodyAndIdleTimeout(t *testing.T) {
	cfg := config.Default()
	cfg.MaxRequestBodyBytes = 3
	cfg.RequestIdleTimeout = 10 * time.Millisecond
	h := RequestLimits(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := r.Body.Read(make([]byte, 8))
		if err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		<-r.Context().Done()
		if !errors.Is(r.Context().Err(), context.DeadlineExceeded) {
			t.Errorf("context error = %v", r.Context().Err())
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/", &slowBody{data: []byte("a")}))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("idle status = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/", &slowBody{data: []byte("abcd")}))
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("body status = %d, want 413", recorder.Code)
	}
}

func cancelledContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

type slowBody struct{ data []byte }

func (b *slowBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		return 0, errors.New("EOF")
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}
func (b *slowBody) Close() error { return nil }
