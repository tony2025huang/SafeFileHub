package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/limits"
	"github.com/example/safefilehub/internal/metrics"
	appLog "github.com/example/safefilehub/internal/observability"
)

// observedHandler applies the one Metrics instance supplied by a deployment to
// every route in a composition. It intentionally owns no process-global state.
func observedHandler(cfg config.Config, mux *http.ServeMux, m *metrics.Metrics) http.Handler {
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(m.Prometheus()))
	})
	base := RequestLimits(cfg, metricResponses(m, mux))
	// Context must be established before logging so request/session correlation
	// and resolved client/peer addresses are present in the end event.
	return requestContext(cfg, applicationLog(cfg, base))
}

// WithApplicationLogger installs an application logger supplied by the process
// composition root. It remains safe for tests and embedded users that use stdout.
func WithApplicationLogger(logger *appLog.MultiLogger, next http.Handler) http.Handler {
	// observedHandler owns the single terminal request event. This wrapper only
	// supplies its sink to that handler (and transfer lifecycle events), so
	// production does not emit duplicate request-end records.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), applicationLoggerKey{}, logger)))
	})
}

type auditRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *auditRecorder) WriteHeader(s int) { w.status = s; w.ResponseWriter.WriteHeader(s) }
func (w *auditRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, e := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, e
}
func applicationLog(cfg config.Config, next http.Handler) http.Handler {
	return applicationLogWithLogger(appLog.NewMulti(appLog.Standard(appLog.Format(cfg.LogFormat))), next)
}
func applicationLogWithLogger(l *appLog.MultiLogger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger := l
		if supplied, _ := r.Context().Value(applicationLoggerKey{}).(*appLog.MultiLogger); supplied != nil {
			logger = supplied
		}
		start := time.Now()
		rw := &auditRecorder{ResponseWriter: w}
		r = r.WithContext(context.WithValue(r.Context(), applicationLoggerKey{}, logger))
		next.ServeHTTP(rw, r)
		if rw.status == 0 {
			rw.status = http.StatusOK
		}
		op := operationFor(r)
		success := rw.status < 400
		userID, _ := r.Context().Value(sessionUserIDKey{}).(int64)
		if userID == 0 {
			if state, _ := r.Context().Value(requestAuditStateKey{}).(*requestAuditState); state != nil {
				userID = state.userID
			}
		}
		logger.Log(appLog.Event{Level: "info", ClientIP: requestClientIP(r), PeerIP: peerIP(r), UserID: userID, RequestID: requestID(r), SessionAuditID: sessionAuditID(r), TransferID: transferID(r), Operation: op, Route: r.URL.Path, Success: success, Status: rw.status, ErrorCode: statusErrorCode(rw.status), Bytes: rw.bytes, DurationMS: time.Since(start).Milliseconds()})
	})
}

func statusErrorCode(status int) string {
	if status >= 400 {
		return http.StatusText(status)
	}
	return ""
}
func operationFor(r *http.Request) string {
	if r.Method == http.MethodPost && r.URL.Path == "/login" {
		return "login"
	}
	if r.Method == http.MethodPost && r.URL.Path == "/logout" {
		return "logout"
	}
	if r.PathValue("id") != "" && strings.HasPrefix(r.URL.Path, "/api/uploads/") {
		if action, ok := map[string]string{http.MethodPatch: "chunk", http.MethodDelete: "cancel", http.MethodHead: "status", http.MethodPost: "complete"}[r.Method]; ok {
			return "upload.request." + action
		}
		return "upload.request"
	}
	if r.URL.Path == "/api/uploads" {
		return "upload.request"
	}
	if r.URL.Path == "/api/files" && r.Method == http.MethodPost {
		return "file.create.request"
	}
	if r.PathValue("fileID") != "" {
		if r.Method == http.MethodDelete {
			return "file.delete.request"
		}
		return "download.request"
	}
	if r.URL.Path == "/api/directories" {
		return "directory.create.request"
	}
	if r.PathValue("directoryID") != "" {
		return "directory.delete.request"
	}
	if r.PathValue("jobID") != "" {
		return "archive.request"
	}
	if len(r.URL.Path) >= len("/api/admin/") && r.URL.Path[:len("/api/admin/")] == "/api/admin/" {
		return "admin"
	}
	if r.PathValue("rootID") != "" && r.Method == http.MethodPost {
		return "archive.request"
	}
	if r.PathValue("rootID") != "" {
		return "file.list"
	}
	return r.Method + " " + r.URL.Path
}

// logTransferLifecycle records a transfer-specific start before the protected
// handler runs and a matching terminal event afterwards. The outer request event
// remains the terminal audit record for login, logout, listing, admin, and
// authorization denials; it is deliberately not relied on for transfers.
func logTransferLifecycle(kind string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger, _ := r.Context().Value(applicationLoggerKey{}).(*appLog.MultiLogger)
		if logger == nil {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		base := appLog.Event{Level: "info", ClientIP: requestClientIP(r), PeerIP: peerIP(r), UserID: userID(r), RequestID: requestID(r), SessionAuditID: sessionAuditID(r), TransferID: transferID(r), Route: r.URL.Path}
		base.Operation = kind + ".start"
		logger.Log(base)
		rw := &auditRecorder{ResponseWriter: w}
		next.ServeHTTP(rw, r)
		if rw.status == 0 {
			rw.status = http.StatusOK
		}
		cancelled := r.Context().Err() != nil
		base.Operation = kind + ".complete"
		base.Status, base.Bytes, base.DurationMS = rw.status, rw.bytes, time.Since(started).Milliseconds()
		base.Success = !cancelled && rw.status < http.StatusBadRequest
		if cancelled {
			base.ErrorCode = "canceled"
			base.Fields = map[string]any{"outcome": "canceled"}
		} else {
			base.ErrorCode = statusErrorCode(rw.status)
		}
		logger.Log(base)
	})
}

func userID(r *http.Request) int64 {
	v, _ := r.Context().Value(sessionUserIDKey{}).(int64)
	return v
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
