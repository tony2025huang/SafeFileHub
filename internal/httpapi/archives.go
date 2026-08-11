package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/example/safefilehub/internal/archive"
	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/pathpolicy"
)

type archiveRepository interface {
	StorageRootByID(context.Context, int64) (db.StorageRoot, error)
	FilesUnderRoot(context.Context, int64) ([]db.File, error)
}
type archiveAuthorizer interface {
	Authorize(context.Context, int64, int64, string, string) (bool, error)
}

// NewServerWithArchives provides job-based archive downloads. Artifacts are
// private temporary files owned by archive.Manager and are never persisted as files.
func NewServerWithArchives(cfg config.Config, users authenticator, sessions sessionManager, repo archiveRepository, authorizer archiveAuthorizer, manager *archive.Manager) (http.Handler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if users == nil || sessions == nil || repo == nil || authorizer == nil || manager == nil {
		return nil, errors.New("archive dependencies are required")
	}
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", healthz)
	m.HandleFunc("POST /login", login(users, sessions))
	m.HandleFunc("POST /logout", logout(sessions))
	m.Handle("POST /api/roots/{rootID}/archives", requireSession(sessions, http.HandlerFunc(createArchive(repo, authorizer, manager, cfg.NamePolicy))))
	m.Handle("GET /api/archives/{jobID}", requireSession(sessions, http.HandlerFunc(downloadArchive(manager))))
	m.Handle("DELETE /api/archives/{jobID}", requireSession(sessions, http.HandlerFunc(cancelArchive(manager))))
	return RequestLimits(cfg, m), nil
}
func createArchive(repo archiveRepository, auth archiveAuthorizer, manager *archive.Manager, policy config.NamePolicy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rootID, err := strconv.ParseInt(r.PathValue("rootID"), 10, 64)
		if err != nil || rootID <= 0 {
			http.Error(w, "invalid root", 400)
			return
		}
		var in struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
			http.Error(w, "invalid archive request", 400)
			return
		}
		decodedPath := strings.TrimPrefix(in.Path, "/")
		if in.Path == "/" {
			decodedPath = "/"
		}
		p, err := pathpolicy.ParseDecodedPath(decodedPath, policy)
		if err != nil {
			http.Error(w, "invalid path", 400)
			return
		}
		uid, _ := r.Context().Value(sessionUserIDKey{}).(int64)
		if uid <= 0 {
			http.Error(w, "unauthorized", 401)
			return
		}
		if _, err := repo.StorageRootByID(r.Context(), rootID); err != nil {
			http.NotFound(w, r)
			return
		}
		files, err := repo.FilesUnderRoot(r.Context(), rootID)
		if err != nil {
			http.Error(w, "load files", 500)
			return
		}
		entries := make([]archive.Entry, 0)
		for _, f := range files {
			if f.LogicalPath == p.Canonical || strings.HasPrefix(f.LogicalPath, strings.TrimSuffix(p.Canonical, "/")+"/") {
				entries = append(entries, archive.Entry{LogicalPath: f.LogicalPath, ObjectKey: f.ObjectKey, Size: f.Size})
			}
		}
		job, err := manager.Create(r.Context(), p.Canonical, entries, archiveHTTPAuthorizer{ctx: r.Context(), auth: auth, uid: uid, rootID: rootID})
		if errors.Is(err, archive.ErrForbidden) {
			http.Error(w, "forbidden", 403)
			return
		}
		if err != nil {
			http.Error(w, "cannot create archive", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(struct {
			ID string `json:"id"`
		}{job.ID})
	}
}

type archiveHTTPAuthorizer struct {
	ctx         context.Context
	auth        archiveAuthorizer
	uid, rootID int64
}

func (a archiveHTTPAuthorizer) Allow(_ context.Context, p string) (bool, error) {
	return a.auth.Authorize(a.ctx, a.uid, a.rootID, p, "archive")
}
func downloadArchive(manager *archive.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		j, err := manager.Job(r.PathValue("jobID"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if j.Status == archive.Running {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "archive pending", http.StatusAccepted)
			return
		}
		if j.Status != archive.Complete {
			http.NotFound(w, r)
			return
		}
		f, err := manager.Open(j.ID)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Length", strconv.FormatInt(j.Size, 10))
		w.Header().Set("Content-Disposition", `attachment; filename="archive.zip"`)
		_, _ = io.CopyBuffer(w, f, make([]byte, 128*1024))
	}
}
func cancelArchive(manager *archive.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := manager.Cancel(r.PathValue("jobID")); err != nil {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
