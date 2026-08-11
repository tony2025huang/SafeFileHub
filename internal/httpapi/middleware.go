package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/limits"
)

// UploadIdentity obtains the authenticated user and canonical client IP for an
// upload request. Upload endpoints can supply their own auth-aware extractor.
type UploadIdentity func(*http.Request) (user, ip string)

// LimitUpload rejects work when any upload capacity limit is reached. It does
// not wait or create goroutines, so saturation cannot create an internal queue.
func LimitUpload(limiter *limits.UploadLimiter, retryAfter time.Duration, identity UploadIdentity, next http.Handler) http.Handler {
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ip := identity(r)
		lease, err := limiter.TryAcquire(user, ip)
		if err != nil {
			w.Header().Set("Retry-After", strconv.FormatInt(int64(retryAfter.Round(time.Second)/time.Second), 10))
			http.Error(w, "upload capacity exceeded; retry later", http.StatusTooManyRequests)
			return
		}
		defer lease.Release()
		next.ServeHTTP(w, r)
	})
}

// RequestLimits bounds body size and handler lifetime. Server read/write and
// idle socket timeouts must additionally be set via ServerTimeouts.
func RequestLimits(cfg config.Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), cfg.RequestIdleTimeout)
		defer cancel()
		r = r.WithContext(ctx)
		r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxRequestBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// ServerTimeouts applies the configured request timeout to socket reads and
// writes for the standard net/http server.
func ServerTimeouts(cfg config.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           RequestLimits(cfg, handler),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.RequestIdleTimeout,
		WriteTimeout:      cfg.RequestIdleTimeout,
		IdleTimeout:       cfg.RequestIdleTimeout,
	}
}
