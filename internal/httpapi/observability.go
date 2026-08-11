package httpapi

import (
	"net/http"
	"time"

	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/limits"
	"github.com/example/safefilehub/internal/metrics"
)

// observedHandler applies the one Metrics instance supplied by a deployment to
// every route in a composition. It intentionally owns no process-global state.
func observedHandler(cfg config.Config, mux *http.ServeMux, m *metrics.Metrics) http.Handler {
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(m.Prometheus()))
	})
	return RequestLimits(cfg, metricResponses(m, mux))
}

func observeUpload(m *metrics.Metrics, next http.Handler) http.Handler {
	return observeActive(m.UploadStarted, m.UploadFinished, m, next)
}
func observeDownload(m *metrics.Metrics, next http.Handler) http.Handler {
	return observeActive(m.DownloadStarted, m.DownloadFinished, m, next)
}
func observeArchive(m *metrics.Metrics, next http.Handler) http.Handler {
	return observeActive(m.ArchiveStarted, m.ArchiveFinished, m, next)
}
func observeActive(start, finish func(), m *metrics.Metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start()
		defer finish()
		next.ServeHTTP(w, r)
		if r.Context().Err() != nil {
			m.IncCancellation()
		}
	})
}

func observeCancellation(m *metrics.Metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if r.Context().Err() != nil {
			m.IncCancellation()
		}
	})
}

func limitUploadWithMetrics(limiter *limits.UploadLimiter, retryAfter time.Duration, identity UploadIdentity, m *metrics.Metrics, next http.Handler) http.Handler {
	return LimitUpload(limiter, retryAfter, identity, observeLease(m, next))
}
func limitDownloadWithMetrics(limiter *limits.DownloadLimiter, m *metrics.Metrics, next http.Handler) http.Handler {
	return limitDownload(limiter, observeLease(m, next))
}
func observeLease(m *metrics.Metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.LeaseStarted()
		defer m.LeaseFinished()
		next.ServeHTTP(w, r)
	})
}
