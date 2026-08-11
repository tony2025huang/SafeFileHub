package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/limits"
)

type UploadIdentity func(*http.Request) (user, ip string)

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

// RequestLimits applies body limits and only an explicitly configured total
// handler deadline. Socket timeouts are independently configured below.
func RequestLimits(cfg config.Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cfg.RequestTimeout > 0 {
			ctx, cancel := context.WithTimeout(r.Context(), cfg.RequestTimeout)
			defer cancel()
			r = r.WithContext(ctx)
		}
		r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxRequestBodyBytes)
		next.ServeHTTP(w, r)
	})
}

func requestTooLarge(err error) bool { var maxErr *http.MaxBytesError; return errors.As(err, &maxErr) }

func ServerTimeouts(cfg config.Config, handler http.Handler) *http.Server {
	return &http.Server{Addr: cfg.ListenAddr, Handler: RequestLimits(cfg, handler), ReadHeaderTimeout: cfg.ReadHeaderTimeout, ReadTimeout: cfg.ReadTimeout, WriteTimeout: cfg.WriteTimeout, IdleTimeout: cfg.IdleTimeout}
}
