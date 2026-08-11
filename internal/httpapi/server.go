// Package httpapi exposes SafeFileHub's HTTP endpoints.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/limits"
	"github.com/example/safefilehub/internal/metrics"
)

// ReadinessChecks are intentionally shallow checks for dependencies needed to
// accept work. They must not enumerate storage directories or object content.
type ReadinessChecks interface {
	Database(*http.Request) error
	Storage(*http.Request) error
	Disk(*http.Request) error
}

// NewServer returns an in-memory HTTP handler configured for SafeFileHub.
func NewServer(cfg config.Config) (http.Handler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate server configuration: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /", transferUI)
	return RequestLimits(cfg, mux), nil
}

// NewServerWithObservability is a minimal constructor for deployments that
// need readiness and metrics before selecting a transfer route composition.
func NewServerWithObservability(cfg config.Config, checks ReadinessChecks, m *metrics.Metrics) (http.Handler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate server configuration: %w", err)
	}
	if checks == nil || m == nil {
		return nil, errors.New("observability dependencies are required")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /readyz", readyz(checks))
	return observedHandler(cfg, mux, m), nil
}

type authenticator interface {
	Authenticate(context.Context, string, string) (db.User, error)
}

type sessionManager interface {
	Create(context.Context, int64) (string, error)
	Logout(context.Context, string) error
	UserID(context.Context, string) (int64, error)
	SetCookie(http.ResponseWriter, string)
	ClearSessionCookie(http.ResponseWriter)
	CookieName() string
}

// NewServerWithAuth adds the minimal Task 4 authentication surface. File
// storage endpoints deliberately remain out of scope until Task 5.
func NewServerWithAuth(cfg config.Config, users authenticator, sessions sessionManager, suppliedLimiter ...*limits.UploadLimiter) (http.Handler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate server configuration: %w", err)
	}
	if users == nil || sessions == nil {
		return nil, errors.New("authentication dependencies are required")
	}
	limiter, err := newUploadLimiter(cfg, suppliedLimiter)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("POST /login", login(users, sessions))
	mux.HandleFunc("POST /logout", logout(sessions))
	mux.Handle("GET /session", requireSession(sessions, http.HandlerFunc(sessionStatus)))
	upload := requireSession(sessions, LimitUpload(limiter, time.Second, sessionUploadIdentity, UploadBodyLimits(cfg.UploadIdleTimeout, 0, http.HandlerFunc(uploadPlaceholder))))
	mux.Handle("POST /api/uploads", upload)
	return RequestLimits(cfg, mux), nil
}

// NewServerWithAuthAndObservability preserves NewServerWithAuth's API while
// allowing a composed deployment to use one caller-owned Metrics instance.
func NewServerWithAuthAndObservability(cfg config.Config, users authenticator, sessions sessionManager, m *metrics.Metrics, suppliedLimiter ...*limits.UploadLimiter) (http.Handler, error) {
	if m == nil {
		return nil, errors.New("observability dependencies are required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate server configuration: %w", err)
	}
	if users == nil || sessions == nil {
		return nil, errors.New("authentication dependencies are required")
	}
	limiter, err := newUploadLimiter(cfg, suppliedLimiter)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("POST /login", login(users, sessions))
	mux.HandleFunc("POST /logout", logout(sessions))
	mux.Handle("GET /session", requireSession(sessions, http.HandlerFunc(sessionStatus)))
	upload := requireSession(sessions, observeUpload(m, limitUploadWithMetrics(limiter, time.Second, sessionUploadIdentity, m, UploadBodyLimits(cfg.UploadIdleTimeout, 0, http.HandlerFunc(uploadPlaceholder)))))
	mux.Handle("POST /api/uploads", upload)
	return observedHandler(cfg, mux, m), nil
}

func newUploadLimiter(cfg config.Config, supplied []*limits.UploadLimiter) (*limits.UploadLimiter, error) {
	if len(supplied) > 1 {
		return nil, errors.New("only one upload limiter may be supplied")
	}
	if len(supplied) == 1 && supplied[0] != nil {
		return supplied[0], nil
	}
	limiter, err := limits.NewUploadLimiter(cfg.UploadConcurrency, cfg.PerUserUploadConcurrency, cfg.PerIPUploadConcurrency)
	if err != nil {
		return nil, fmt.Errorf("create upload limiter: %w", err)
	}
	return limiter, nil
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func login(users authenticator, sessions sessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input loginRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		user, err := users.Authenticate(r.Context(), input.Username, input.Password)
		if err != nil {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		id, err := sessions.Create(r.Context(), user.ID)
		if err != nil {
			http.Error(w, "create session", http.StatusInternalServerError)
			return
		}
		sessions.SetCookie(w, id)
		w.WriteHeader(http.StatusNoContent)
	}
}

func logout(sessions sessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie(sessions.CookieName()); err == nil {
			if err := sessions.Logout(r.Context(), cookie.Value); err != nil {
				http.Error(w, "revoke session", http.StatusInternalServerError)
				return
			}
		}
		sessions.ClearSessionCookie(w)
		w.WriteHeader(http.StatusNoContent)
	}
}

type sessionUserIDKey struct{}

func requireSession(sessions sessionManager, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessions.CookieName())
		if err != nil || cookie.Value == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		userID, err := sessions.UserID(r.Context(), cookie.Value)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if userID <= 0 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), sessionUserIDKey{}, userID)
		// Correlate session activity without recording the bearer cookie itself.
		auditID := sessionAuditToken(cookie.Value)
		ctx = context.WithValue(ctx, sessionAuditIDKey{}, auditID)
		// The outer terminal request logger sees the original request context, so
		// retain only safe correlation data in its request-scoped shared state.
		if state, _ := r.Context().Value(requestAuditStateKey{}).(*requestAuditState); state != nil {
			state.userID, state.sessionAuditID = userID, auditID
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func sessionStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("authenticated\n"))
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func readyz(checks ReadinessChecks) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if checks.Database(r) != nil || checks.Storage(r) != nil || checks.Disk(r) != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ready\n"))
	}
}

func sessionUploadIdentity(r *http.Request) (string, string) {
	userID, _ := r.Context().Value(sessionUserIDKey{}).(int64)
	ip, _ := clientIP(r)
	return strconv.FormatInt(userID, 10), ip
}

// clientIP trusts only the socket peer address. Forwarded headers remain
// untrusted until an explicit trusted-proxy configuration exists.
func clientIP(r *http.Request) (string, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "", false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", false
	}
	return ip.String(), true
}

// uploadPlaceholder intentionally implements no upload protocol; it exercises
// admission, session and body-size protections for future upload endpoints.
func uploadPlaceholder(w http.ResponseWriter, r *http.Request) {
	_, err := io.Copy(io.Discard, r.Body)
	if requestTooLarge(err) {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		http.Error(w, "read upload request", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
