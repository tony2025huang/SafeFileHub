package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/limits"
	"github.com/example/safefilehub/internal/pathpolicy"
	"github.com/example/safefilehub/internal/storage"
	"github.com/example/safefilehub/internal/upload"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type uploadRepository interface {
	StorageRootByID(context.Context, int64) (db.StorageRoot, error)
	FilesUnderRoot(context.Context, int64) ([]db.File, error)
	FileByRootAndPath(context.Context, int64, string) (db.File, error)
	CreateUploadSession(context.Context, db.UploadSession) (db.UploadSession, error)
	UploadSessionByID(context.Context, string) (db.UploadSession, error)
	UpdateUploadOffset(context.Context, string, int64, int64) error
	UpdateUploadStatus(context.Context, string, string, string) error
	DeleteUploadSession(context.Context, string) error
}
type uploadAuthorizer interface {
	Authorize(context.Context, int64, int64, string, string) (bool, error)
}
type uploadRequest struct {
	RootID int64  `json:"root_id"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
}
type uploadResponse struct {
	UploadID  string    `json:"upload_id"`
	ChunkSize int64     `json:"chunk_size"`
	Offset    int64     `json:"offset"`
	Expires   time.Time `json:"expires"`
}

func NewServerWithUploads(cfg config.Config, users authenticator, sessions sessionManager, repo uploadRepository, authorizer uploadAuthorizer, store *storage.ObjectStore, supplied ...*limits.UploadLimiter) (http.Handler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if users == nil || sessions == nil || repo == nil || authorizer == nil || store == nil {
		return nil, errors.New("upload dependencies are required")
	}
	limiter, err := newUploadLimiter(cfg, supplied)
	if err != nil {
		return nil, err
	}
	m := upload.New(repo, store, cfg.ChunkSize, cfg.UploadSessionTTL)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("POST /login", login(users, sessions))
	mux.HandleFunc("POST /logout", logout(sessions))
	mux.Handle("GET /session", requireSession(sessions, http.HandlerFunc(sessionStatus)))
	// The production constructor composes the Task 5 listing surface with
	// upload endpoints, rather than replacing it with an upload-only mux.
	mux.Handle("GET /roots/{rootID}/files", requireSession(sessions, listFiles(repo, authorizer, cfg.NamePolicy)))
	create := requireSession(sessions, http.HandlerFunc(createUpload(m, repo, authorizer, cfg)))
	mux.Handle("POST /api/uploads", create)
	state := requireSession(sessions, http.HandlerFunc(uploadState(m, authorizer)))
	patch := requireSession(sessions, LimitUpload(limiter, time.Second, sessionUploadIdentity, UploadBodyLimits(cfg.UploadIdleTimeout, cfg.MaxRequestBodyBytes, http.HandlerFunc(patchUpload(m, authorizer)))))
	mux.Handle("HEAD /api/uploads/{id}", state)
	mux.Handle("DELETE /api/uploads/{id}", state)
	mux.Handle("PATCH /api/uploads/{id}", patch)
	return RequestLimits(cfg, mux), nil
}
func createUpload(m *upload.Manager, repo uploadRepository, a uploadAuthorizer, cfg config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in uploadRequest
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		d.DisallowUnknownFields()
		if err := d.Decode(&in); err != nil || in.RootID <= 0 || in.Size < 0 || in.Size > cfg.MaxRequestBodyBytes {
			http.Error(w, "invalid upload", http.StatusBadRequest)
			return
		}
		p, err := pathpolicy.ParseDecodedPath(in.Path, cfg.NamePolicy)
		if err != nil {
			http.Error(w, "invalid path", 400)
			return
		}
		uid := r.Context().Value(sessionUserIDKey{}).(int64)
		ok, err := a.Authorize(r.Context(), uid, in.RootID, p.Canonical, "write")
		if err != nil {
			http.Error(w, "authorize upload", 500)
			return
		}
		if !ok {
			http.Error(w, "forbidden", 403)
			return
		}
		if _, err = repo.StorageRootByID(r.Context(), in.RootID); err != nil {
			http.Error(w, "not found", 404)
			return
		}
		if _, err = repo.FileByRootAndPath(r.Context(), in.RootID, p.Canonical); err == nil {
			http.Error(w, "destination exists", 409)
			return
		} else if !errors.Is(err, db.ErrNotFound) {
			http.Error(w, "check destination", 500)
			return
		}
		s, err := m.Create(r.Context(), uid, in.RootID, p.Canonical, in.Size)
		if err != nil {
			http.Error(w, "create upload", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(uploadResponse{s.ID, cfg.ChunkSize, s.Offset, s.ExpiresAt})
	}
}
func sessionFor(r *http.Request, m *upload.Manager, a uploadAuthorizer, action string) (db.UploadSession, bool) {
	s, err := m.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		return s, false
	}
	uid := r.Context().Value(sessionUserIDKey{}).(int64)
	// Ownership grants access under the upload-session policy. Collaborators
	// need the permission matching the requested operation.
	if uid != s.UserID {
		ok, e := a.Authorize(r.Context(), uid, s.RootID, s.LogicalPath, action)
		if e != nil || !ok {
			return s, false
		}
	}
	return s, true
}
func uploadState(m *upload.Manager, a uploadAuthorizer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		action := "read"
		if r.Method == http.MethodDelete {
			action = "delete"
		}
		s, ok := sessionFor(r, m, a, action)
		if !ok {
			http.Error(w, "not found", 404)
			return
		}
		if r.Method == http.MethodDelete {
			if err := m.Cancel(r.Context(), s.ID); err != nil {
				http.Error(w, "cancel upload", 500)
				return
			}
			w.WriteHeader(204)
			return
		}
		w.Header().Set("Upload-Offset", strconv.FormatInt(s.Offset, 10))
		w.Header().Set("Upload-Length", strconv.FormatInt(s.Length, 10))
		w.WriteHeader(200)
	}
}
func patchUpload(m *upload.Manager, a uploadAuthorizer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]), "application/offset+octet-stream") {
			http.Error(w, "invalid content type", 415)
			return
		}
		s, ok := sessionFor(r, m, a, "write")
		if !ok {
			http.Error(w, "not found", 404)
			return
		}
		off, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
		if err != nil || off < 0 {
			http.Error(w, "invalid offset", 400)
			return
		}
		next, err := m.Write(r.Context(), s.ID, off, r.Body)
		if errors.Is(err, upload.ErrOffset) {
			w.Header().Set("Upload-Offset", strconv.FormatInt(next, 10))
			http.Error(w, "offset conflict", 409)
			return
		}
		if errors.Is(err, upload.ErrTooLarge) {
			http.Error(w, "upload too large", 413)
			return
		}
		if err != nil {
			http.Error(w, fmt.Sprintf("write upload: %v", err), 400)
			return
		}
		w.Header().Set("Upload-Offset", strconv.FormatInt(next, 10))
		w.WriteHeader(204)
	}
}
