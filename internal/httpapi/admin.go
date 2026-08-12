package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/example/safefilehub/internal/auth"
	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/pathpolicy"
)

type adminRepository interface {
	UserByID(context.Context, int64) (db.User, error)
	IsBootstrapAdmin(context.Context, int64) (bool, error)
	CreateUser(context.Context, db.User) (db.User, error)
	UpdateUserCredentials(context.Context, int64, string, bool) error
	StorageRootByID(context.Context, int64) (db.StorageRoot, error)
	CreatePermission(context.Context, db.Permission) (db.Permission, error)
	CreateAuditEvent(context.Context, db.AuditEvent) (db.AuditEvent, error)
}
type userSessionRevoker interface {
	sessionManager
	RevokeUser(context.Context, int64) error
}

type adminUserResponse struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Disabled bool   `json:"disabled"`
}
type createAdminUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}
type setDisabledRequest struct {
	Disabled bool `json:"disabled"`
}
type resetPasswordRequest struct {
	Password string `json:"password"`
}
type setPermissionRequest struct {
	RootID     int64  `json:"root_id"`
	PathPrefix string `json:"path_prefix"`
	Action     string `json:"action"`
	Allow      bool   `json:"allow"`
}

// NewServerWithAdmin exposes the authenticated administration API. Deployments
// composing transfer routes may register registerAdminRoutes on their existing mux.
func NewServerWithAdmin(cfg config.Config, users authenticator, sessions userSessionRevoker, repository adminRepository) (http.Handler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate server configuration: %w", err)
	}
	if users == nil || sessions == nil || repository == nil {
		return nil, errors.New("admin dependencies are required")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("POST /login", login(users, sessions))
	mux.HandleFunc("POST /logout", logout(sessions))
	mux.Handle("GET /session", requireSession(sessions, http.HandlerFunc(sessionStatus)))
	registerAdminRoutes(mux, cfg, sessions, repository)
	return RequestLimits(cfg, mux), nil
}

func registerAdminRoutes(mux *http.ServeMux, cfg config.Config, sessions userSessionRevoker, repository adminRepository) {
	guard := func(next http.Handler) http.Handler {
		return requireSession(sessions, requireAdmin(cfg, repository, next))
	}
	mux.Handle("POST /api/admin/users", guard(http.HandlerFunc(createAdminUser(repository))))
	mux.Handle("PUT /api/admin/users/{userID}/disabled", guard(http.HandlerFunc(setUserDisabled(repository, sessions))))
	mux.Handle("PUT /api/admin/users/{userID}/password", guard(http.HandlerFunc(resetUserPassword(repository, sessions))))
	mux.Handle("PUT /api/admin/users/{userID}/permissions", guard(http.HandlerFunc(setUserPermission(cfg.NamePolicy, repository))))
	mux.Handle("GET /api/admin/audit", guard(http.HandlerFunc(listAuditEvents(repository))))
}

func requireAdmin(cfg config.Config, repo adminRepository, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := r.Context().Value(sessionUserIDKey{}).(int64)
		u, err := repo.UserByID(r.Context(), id)
		bootstrap, bootstrapErr := repo.IsBootstrapAdmin(r.Context(), id)
		if err != nil || bootstrapErr != nil || !isAdmin(cfg.AdminUsernames, u, bootstrap) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isAdmin recognizes the explicitly recorded bootstrap administrator. The
// configured usernames remain supported for explicitly provisioned admins.
func isAdmin(names []string, user db.User, bootstrap bool) bool {
	if bootstrap {
		return true
	}
	for _, name := range names {
		if name == user.Username {
			return true
		}
	}
	return false
}
func adminActor(r *http.Request) int64 {
	id, _ := r.Context().Value(sessionUserIDKey{}).(int64)
	return id
}
func adminTargetID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("userID"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid user")
	}
	return id, nil
}
func decodeAdminJSON(w http.ResponseWriter, r *http.Request, value any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	return d.Decode(value)
}
func auditAdmin(ctx context.Context, repo adminRepository, actor, target int64, action, path string) error {
	_, err := repo.CreateAuditEvent(ctx, db.AuditEvent{UserID: actor, Action: action, LogicalPath: path, Detail: "target_user_id=" + strconv.FormatInt(target, 10)})
	return err
}

type auditResponse struct {
	Actor     int64  `json:"actor"`
	Action    string `json:"action"`
	Path      string `json:"path"`
	Status    int    `json:"status"`
	CreatedAt string `json:"created_at"`
}

func listAuditEvents(repo adminRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auditRepo, ok := repo.(interface {
			AuditEvents(context.Context, int) ([]db.AuditEvent, error)
		})
		if !ok {
			http.Error(w, "audit unavailable", http.StatusServiceUnavailable)
			return
		}
		limit := 100
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > 100 {
				http.Error(w, "invalid limit", 400)
				return
			}
			limit = n
		}
		events, err := auditRepo.AuditEvents(r.Context(), limit)
		if err != nil {
			http.Error(w, "audit", 500)
			return
		}
		result := make([]auditResponse, 0, len(events))
		for _, event := range events {
			result = append(result, auditResponse{Actor: event.UserID, Action: event.Action, Path: event.LogicalPath, Status: event.Status, CreatedAt: event.CreatedAt.UTC().Format(time.RFC3339Nano)})
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(result)
	}
}

func createAdminUser(repo adminRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in createAdminUserRequest
		if err := decodeAdminJSON(w, r, &in); err != nil || strings.TrimSpace(in.Username) == "" || in.Password == "" {
			http.Error(w, "invalid user", http.StatusBadRequest)
			return
		}
		hash, err := auth.HashPassword(in.Password)
		if err != nil {
			http.Error(w, "invalid user", http.StatusBadRequest)
			return
		}
		u, err := repo.CreateUser(r.Context(), db.User{Username: strings.TrimSpace(in.Username), PasswordHash: hash})
		if err != nil {
			if errors.Is(err, db.ErrConflict) {
				http.Error(w, "user exists", http.StatusConflict)
			} else {
				http.Error(w, "create user", 500)
			}
			return
		}
		if err := auditAdmin(r.Context(), repo, adminActor(r), u.ID, "admin.user.create", "/"); err != nil {
			http.Error(w, "audit", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(adminUserResponse{ID: u.ID, Username: u.Username, Disabled: u.Disabled})
	}
}
func setUserDisabled(repo adminRepository, sessions userSessionRevoker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := adminTargetID(r)
		if err != nil {
			http.Error(w, "invalid user", 400)
			return
		}
		var in setDisabledRequest
		if err := decodeAdminJSON(w, r, &in); err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		u, err := repo.UserByID(r.Context(), id)
		if err != nil {
			http.Error(w, "not found", 404)
			return
		}
		if err := repo.UpdateUserCredentials(r.Context(), id, u.PasswordHash, in.Disabled); err != nil {
			http.Error(w, "update user", 500)
			return
		}
		if in.Disabled {
			if err := sessions.RevokeUser(r.Context(), id); err != nil {
				http.Error(w, "revoke sessions", 500)
				return
			}
		}
		if err := auditAdmin(r.Context(), repo, adminActor(r), id, "admin.user.disabled", "/"); err != nil {
			http.Error(w, "audit", 500)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
func resetUserPassword(repo adminRepository, sessions userSessionRevoker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := adminTargetID(r)
		if err != nil {
			http.Error(w, "invalid user", 400)
			return
		}
		var in resetPasswordRequest
		if err := decodeAdminJSON(w, r, &in); err != nil || in.Password == "" {
			http.Error(w, "invalid request", 400)
			return
		}
		u, err := repo.UserByID(r.Context(), id)
		if err != nil {
			http.Error(w, "not found", 404)
			return
		}
		hash, err := auth.HashPassword(in.Password)
		if err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		if err := repo.UpdateUserCredentials(r.Context(), id, hash, u.Disabled); err != nil {
			http.Error(w, "update user", 500)
			return
		}
		if err := sessions.RevokeUser(r.Context(), id); err != nil {
			http.Error(w, "revoke sessions", 500)
			return
		}
		if err := auditAdmin(r.Context(), repo, adminActor(r), id, "admin.user.password_reset", "/"); err != nil {
			http.Error(w, "audit", 500)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
func setUserPermission(policy config.NamePolicy, repo adminRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := adminTargetID(r)
		if err != nil {
			http.Error(w, "invalid user", 400)
			return
		}
		var in setPermissionRequest
		if err := decodeAdminJSON(w, r, &in); err != nil || in.RootID <= 0 {
			http.Error(w, "invalid permission", 400)
			return
		}
		if _, err := repo.UserByID(r.Context(), id); err != nil {
			http.Error(w, "not found", 404)
			return
		}
		if _, err := repo.StorageRootByID(r.Context(), in.RootID); err != nil {
			http.Error(w, "not found", 404)
			return
		}
		prefix, err := canonicalAdminPrefix(in.PathPrefix, policy)
		if err != nil {
			http.Error(w, "invalid path prefix", 400)
			return
		}
		if _, err := repo.CreatePermission(r.Context(), db.Permission{UserID: id, RootID: in.RootID, PathPrefix: prefix, Action: in.Action, Allow: in.Allow}); err != nil {
			http.Error(w, "invalid permission", 400)
			return
		}
		if err := auditAdmin(r.Context(), repo, adminActor(r), id, "admin.permission.set", prefix); err != nil {
			http.Error(w, "audit", 500)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
func canonicalAdminPrefix(value string, policy config.NamePolicy) (string, error) {
	if value == "/" {
		return "/", nil
	}
	if !strings.HasPrefix(value, "/") {
		return "", errors.New("absolute required")
	}
	p, err := pathpolicy.ParseEscapedPath(strings.TrimPrefix(value, "/"), policy)
	if err != nil || p.Canonical != value {
		return "", errors.New("invalid prefix")
	}
	return p.Canonical, nil
}
