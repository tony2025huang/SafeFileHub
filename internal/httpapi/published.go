package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strconv"

	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/pathpolicy"
	"github.com/example/safefilehub/internal/storage"
)

type publishedAuditRepository interface {
	CreateAuditEvent(context.Context, db.AuditEvent) (db.AuditEvent, error)
}

func auditPublished(repo publishedRepository, r *http.Request, action string, userID, rootID int64, logicalPath string, status int) {
	a, ok := repo.(publishedAuditRepository)
	if !ok {
		return
	}
	result := "failure"
	if status >= 200 && status < 400 {
		result = "success"
	}
	// IDs are opaque correlation tokens already emitted by the application logger.
	_, _ = a.CreateAuditEvent(r.Context(), db.AuditEvent{UserID: userID, RootID: rootID, Action: action, LogicalPath: logicalPath, Status: status, Detail: "status=" + strconv.Itoa(status) + " result=" + result + " request_id=" + requestID(r) + " session_audit_id=" + sessionAuditID(r)})
}

type publishedRepository interface {
	StorageRootByID(context.Context, int64) (db.StorageRoot, error)
	FileByID(context.Context, int64) (db.File, error)
	FileByRootAndPath(context.Context, int64, string) (db.File, error)
	CreateFile(context.Context, db.File) (db.File, error)
	DeleteFile(context.Context, int64) error
	BeginFileDelete(context.Context, int64, string) error
	FinalizeFileDelete(context.Context, int64, string) error
	EnqueueObjectCleanup(context.Context, string, string) error
	CompleteObjectCleanup(context.Context, string) error
	CreateDirectory(context.Context, db.Directory) (db.Directory, error)
	DirectoryByID(context.Context, int64) (db.Directory, error)
	DirectoryByRootAndPath(context.Context, int64, string) (db.Directory, error)
	DirectoryEmpty(context.Context, db.Directory) (bool, error)
	DeleteDirectory(context.Context, int64) error
}
type directoryRequest struct {
	RootID int64  `json:"root_id"`
	Path   string `json:"path"`
}
type directoryResponse struct {
	ID     int64  `json:"id"`
	RootID int64  `json:"root_id"`
	Path   string `json:"path"`
}

type fileResponse struct {
	ID     int64  `json:"id"`
	RootID int64  `json:"root_id"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
}

// createEmptyFile publishes a durable zero-byte object before recording its
// metadata. If metadata cannot be inserted, it removes that object, so no
// failed request leaves a reachable object or a partial files row.
func createEmptyFile(repo publishedRepository, a fileAuthorizer, policy config.NamePolicy, store *storage.ObjectStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rw := &auditRecorder{ResponseWriter: w}
		w = rw
		var auditUser, auditRoot int64
		var auditPath string
		auditUser, _ = r.Context().Value(sessionUserIDKey{}).(int64)
		defer func() {
			status := rw.status
			if status == 0 {
				status = http.StatusOK
			}
			auditPublished(repo, r, "file.create", auditUser, auditRoot, auditPath, status)
		}()
		var in directoryRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&in) != nil || in.RootID <= 0 {
			http.Error(w, "invalid file", http.StatusBadRequest)
			return
		}
		p, err := pathpolicy.ParseDecodedPath(in.Path, policy)
		if err != nil || p.Canonical == "/" {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		auditRoot, auditPath = in.RootID, p.Canonical
		uid, ok := r.Context().Value(sessionUserIDKey{}).(int64)
		if !ok || uid <= 0 {
			http.Error(w, "unauthorized", 401)
			return
		}
		allowed, err := a.Authorize(r.Context(), uid, in.RootID, p.Canonical, "write")
		if err != nil {
			http.Error(w, "authorize file", 500)
			return
		}
		if !allowed {
			http.Error(w, "forbidden", 403)
			return
		}
		if _, err = repo.StorageRootByID(r.Context(), in.RootID); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				http.Error(w, "not found", 404)
			} else {
				http.Error(w, "load root", 500)
			}
			return
		}
		parent := path.Dir(p.Canonical)
		if parent != "/" {
			if _, err = repo.DirectoryByRootAndPath(r.Context(), in.RootID, parent); err != nil {
				if errors.Is(err, db.ErrNotFound) {
					http.Error(w, "parent directory not found", 404)
				} else {
					http.Error(w, "load parent directory", 500)
				}
				return
			}
		}
		if _, err = repo.FileByRootAndPath(r.Context(), in.RootID, p.Canonical); err == nil {
			http.Error(w, "destination exists", 409)
			return
		} else if !errors.Is(err, db.ErrNotFound) {
			http.Error(w, "check destination", 500)
			return
		}
		key, err := store.CreateEmpty(p.Canonical)
		if err != nil {
			http.Error(w, "create object", 500)
			return
		}
		out, err := repo.CreateFile(r.Context(), db.File{RootID: in.RootID, LogicalPath: p.Canonical, ObjectKey: key, Size: 0, CreatedByUserID: uid})
		if err != nil {
			// The cleanup record is written before the best-effort removal, so a
			// crash or Remove failure never loses recovery information.
			if cleanupErr := repo.EnqueueObjectCleanup(r.Context(), key, "empty_file_metadata_failed"); cleanupErr != nil {
				http.Error(w, "create metadata; record cleanup", 500)
				return
			}
			if removeErr := store.Remove(key); removeErr == nil {
				_ = repo.CompleteObjectCleanup(r.Context(), key)
			} else {
				http.Error(w, "create metadata; cleanup pending", 500)
				return
			}
			if errors.Is(err, db.ErrConflict) {
				http.Error(w, "destination exists", 409)
			} else {
				http.Error(w, "create metadata", 500)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(fileResponse{ID: out.ID, RootID: out.RootID, Path: out.LogicalPath, Size: out.Size})
	}
}

func createDirectory(repo publishedRepository, a fileAuthorizer, policy config.NamePolicy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rw := &auditRecorder{ResponseWriter: w}
		w = rw
		var auditUser, auditRoot int64
		var auditPath string
		auditUser, _ = r.Context().Value(sessionUserIDKey{}).(int64)
		defer func() {
			status := rw.status
			if status == 0 {
				status = http.StatusOK
			}
			auditPublished(repo, r, "directory.create", auditUser, auditRoot, auditPath, status)
		}()
		var in directoryRequest
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		d.DisallowUnknownFields()
		if d.Decode(&in) != nil || in.RootID <= 0 {
			http.Error(w, "invalid directory", 400)
			return
		}
		p, err := pathpolicy.ParseDecodedPath(in.Path, policy)
		if err != nil || p.Canonical == "/" {
			http.Error(w, "invalid path", 400)
			return
		}
		auditRoot, auditPath = in.RootID, p.Canonical
		uid, ok := r.Context().Value(sessionUserIDKey{}).(int64)
		if !ok || uid <= 0 {
			http.Error(w, "unauthorized", 401)
			return
		}
		allowed, err := a.Authorize(r.Context(), uid, in.RootID, p.Canonical, "write")
		if err != nil {
			http.Error(w, "authorize directory", 500)
			return
		}
		if !allowed {
			http.Error(w, "forbidden", 403)
			return
		}
		if _, err = repo.StorageRootByID(r.Context(), in.RootID); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				http.Error(w, "not found", 404)
			} else {
				http.Error(w, "load root", 500)
			}
			return
		}
		parent := path.Dir(p.Canonical)
		if parent != "/" {
			if _, err = repo.DirectoryByRootAndPath(r.Context(), in.RootID, parent); err != nil {
				if errors.Is(err, db.ErrNotFound) {
					http.Error(w, "parent directory not found", 404)
				} else {
					http.Error(w, "load parent directory", 500)
				}
				return
			}
		}
		// The DB trigger is authoritative for cross-type races; this preflight
		// only improves the normal error response.
		if _, err = repo.FileByRootAndPath(r.Context(), in.RootID, p.Canonical); err == nil {
			http.Error(w, "destination exists", 409)
			return
		} else if !errors.Is(err, db.ErrNotFound) {
			http.Error(w, "check destination", 500)
			return
		}
		out, err := repo.CreateDirectory(r.Context(), db.Directory{RootID: in.RootID, LogicalPath: p.Canonical, CreatedByUserID: uid})
		if err != nil {
			if errors.Is(err, db.ErrConflict) {
				http.Error(w, "destination exists", 409)
			} else {
				http.Error(w, "create directory", 500)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(directoryResponse{out.ID, out.RootID, out.LogicalPath})
	}
}
func deleteFile(repo publishedRepository, a fileAuthorizer, store *storage.ObjectStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rw := &auditRecorder{ResponseWriter: w}
		w = rw
		var auditUser, auditRoot int64
		var auditPath string
		auditUser, _ = r.Context().Value(sessionUserIDKey{}).(int64)
		defer func() {
			status := rw.status
			if status == 0 {
				status = http.StatusOK
			}
			auditPublished(repo, r, "file.delete", auditUser, auditRoot, auditPath, status)
		}()
		id, err := strconv.ParseInt(r.PathValue("fileID"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "invalid file", 400)
			return
		}
		f, err := repo.FileByID(r.Context(), id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				http.Error(w, "not found", 404)
			} else {
				http.Error(w, "load file", 500)
			}
			return
		}
		auditRoot, auditPath = f.RootID, f.LogicalPath
		uid, authenticated := r.Context().Value(sessionUserIDKey{}).(int64)
		if !authenticated || uid <= 0 {
			http.Error(w, "unauthorized", 401)
			return
		}
		ok, err := a.Authorize(r.Context(), uid, f.RootID, f.LogicalPath, "delete")
		if err != nil {
			http.Error(w, "authorize delete", 500)
			return
		}
		if !ok {
			http.Error(w, "forbidden", 403)
			return
		}
		if err = repo.BeginFileDelete(r.Context(), id, f.ObjectKey); err != nil {
			http.Error(w, "delete changed", 409)
			return
		}
		if err = store.Remove(f.ObjectKey); err != nil {
			// Tombstone remains invisible and retryable; no metadata is lost.
			http.Error(w, "remove object; deletion pending", 500)
			return
		}
		if err = repo.FinalizeFileDelete(r.Context(), id, f.ObjectKey); err != nil {
			http.Error(w, "finalize metadata; deletion pending", 500)
			return
		}
		w.WriteHeader(204)
	}
}
func deleteDirectory(repo publishedRepository, a fileAuthorizer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rw := &auditRecorder{ResponseWriter: w}
		w = rw
		var auditUser, auditRoot int64
		var auditPath string
		auditUser, _ = r.Context().Value(sessionUserIDKey{}).(int64)
		defer func() {
			status := rw.status
			if status == 0 {
				status = http.StatusOK
			}
			auditPublished(repo, r, "directory.delete", auditUser, auditRoot, auditPath, status)
		}()
		id, err := strconv.ParseInt(r.PathValue("directoryID"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "invalid directory", 400)
			return
		}
		d, err := repo.DirectoryByID(r.Context(), id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				http.Error(w, "not found", 404)
			} else {
				http.Error(w, "load directory", 500)
			}
			return
		}
		auditRoot, auditPath = d.RootID, d.LogicalPath
		uid, authenticated := r.Context().Value(sessionUserIDKey{}).(int64)
		if !authenticated || uid <= 0 {
			http.Error(w, "unauthorized", 401)
			return
		}
		ok, err := a.Authorize(r.Context(), uid, d.RootID, d.LogicalPath, "delete")
		if err != nil {
			http.Error(w, "authorize delete", 500)
			return
		}
		if !ok {
			http.Error(w, "forbidden", 403)
			return
		}
		empty, err := repo.DirectoryEmpty(r.Context(), d)
		if err != nil {
			http.Error(w, "check directory", 500)
			return
		}
		if !empty {
			http.Error(w, "directory not empty", 409)
			return
		}
		if err = repo.DeleteDirectory(r.Context(), id); err != nil {
			if errors.Is(err, db.ErrConflict) {
				http.Error(w, "directory not empty", 409)
			} else {
				http.Error(w, "remove directory", 500)
			}
			return
		}
		w.WriteHeader(204)
	}
}
