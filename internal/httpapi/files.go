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

	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/pathpolicy"
)

type fileRepository interface {
	StorageRootByID(context.Context, int64) (db.StorageRoot, error)
	FilesUnderRoot(context.Context, int64) ([]db.File, error)
}
type directoryListRepository interface {
	DirectoriesUnderRoot(context.Context, int64) ([]db.Directory, error)
}
type fileAuthorizer interface {
	Authorize(context.Context, int64, int64, string, string) (bool, error)
}

type fileListResponse struct {
	Path  string          `json:"path"`
	Files []fileListEntry `json:"files"`
}
type fileListEntry struct {
	Name        string    `json:"name"`
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	MD5Status   string    `json:"md5_status"`
	MD5Digest   string    `json:"md5_digest"`
	IsDirectory bool      `json:"is_directory"`
	ID          int64     `json:"id"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// NewServerWithFiles adds the Task 5 authenticated logical directory listing.
func NewServerWithFiles(cfg config.Config, users authenticator, sessions sessionManager, repository fileRepository, authorizer fileAuthorizer) (http.Handler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate server configuration: %w", err)
	}
	if users == nil || sessions == nil || repository == nil || authorizer == nil {
		return nil, errors.New("file listing dependencies are required")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("POST /login", login(users, sessions))
	mux.HandleFunc("POST /logout", logout(sessions))
	mux.Handle("GET /session", requireSession(sessions, http.HandlerFunc(sessionStatus)))
	mux.Handle("GET /roots/{rootID}/files", requireSession(sessions, listFiles(repository, authorizer, cfg.NamePolicy)))
	return mux, nil
}

func listFiles(repository fileRepository, authorizer fileAuthorizer, policy config.NamePolicy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rootID, err := strconv.ParseInt(r.PathValue("rootID"), 10, 64)
		if err != nil || rootID <= 0 {
			http.Error(w, "invalid root", http.StatusBadRequest)
			return
		}
		path, err := pathpolicy.ParseDecodedPath(r.URL.Query().Get("path"), policy)
		if err != nil {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		userID, ok := r.Context().Value(sessionUserIDKey{}).(int64)
		if !ok || userID <= 0 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		allowed, err := authorizer.Authorize(r.Context(), userID, rootID, path.Canonical, "read")
		if err != nil {
			http.Error(w, "authorize listing", http.StatusInternalServerError)
			return
		}
		if !allowed {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if _, err := repository.StorageRootByID(r.Context(), rootID); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				http.Error(w, "not found", http.StatusNotFound)
			} else {
				http.Error(w, "load root", http.StatusInternalServerError)
			}
			return
		}
		files, err := repository.FilesUnderRoot(r.Context(), rootID)
		if err != nil {
			http.Error(w, "list files", http.StatusInternalServerError)
			return
		}
		var directories []db.Directory
		if dr, ok := repository.(directoryListRepository); ok {
			directories, err = dr.DirectoriesUnderRoot(r.Context(), rootID)
			if err != nil {
				http.Error(w, "list directories", http.StatusInternalServerError)
				return
			}
		}
		response := fileListResponse{Path: path.Canonical, Files: make([]fileListEntry, 0)}
		for _, file := range files {
			if !strings.HasPrefix(file.ObjectKey, "objects/") || !isDirectChild(path.Canonical, file.LogicalPath) {
				continue
			}
			parsed, err := logicalCanonical(file.LogicalPath, policy)
			if err != nil || parsed != file.LogicalPath {
				continue
			}
			allowed, err := authorizer.Authorize(r.Context(), userID, rootID, file.LogicalPath, "read")
			if err != nil || !allowed {
				continue
			}
			prefix := path.Canonical + "/"
			if path.Canonical == "/" {
				prefix = "/"
			}
			response.Files = append(response.Files, fileListEntry{Name: strings.TrimPrefix(file.LogicalPath, prefix), Path: file.LogicalPath, Size: file.Size, ID: file.ID, UpdatedAt: file.UpdatedAt, MD5Status: publicMD5Status(file.MD5Status), MD5Digest: publicMD5Digest(file.MD5Status, file.MD5Digest)})
		}
		for _, d := range directories {
			if !isDirectChild(path.Canonical, d.LogicalPath) {
				continue
			}
			allowed, err := authorizer.Authorize(r.Context(), userID, rootID, d.LogicalPath, "read")
			if err != nil || !allowed {
				continue
			}
			prefix := path.Canonical + "/"
			if path.Canonical == "/" {
				prefix = "/"
			}
			response.Files = append(response.Files, fileListEntry{Name: strings.TrimPrefix(d.LogicalPath, prefix), Path: d.LogicalPath, IsDirectory: true, ID: d.ID, UpdatedAt: d.UpdatedAt})
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(response)
	}
}

// publicMD5Status and publicMD5Digest deliberately omit MD5Error: repository
// errors may contain object keys or host paths and are never API data.
func publicMD5Status(status string) string {
	switch status {
	case db.MD5Disabled, db.MD5Pending, db.MD5Computing, db.MD5Ready, db.MD5Failed:
		return status
	default:
		return db.MD5Disabled
	}
}
func publicMD5Digest(status, digest string) string {
	if status != db.MD5Ready || len(digest) != 32 {
		return ""
	}
	for _, c := range digest {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return ""
		}
	}
	return digest
}

func logicalCanonical(value string, policy config.NamePolicy) (string, error) {
	if value == "/" {
		return "/", nil
	}
	if !strings.HasPrefix(value, "/") {
		return "", errors.New("not absolute logical path")
	}
	parsed, err := pathpolicy.ParseDecodedPath(strings.TrimPrefix(value, "/"), policy)
	if err != nil {
		return "", err
	}
	return parsed.Canonical, nil
}
func isDirectChild(directory, candidate string) bool {
	prefix := directory
	if prefix != "/" {
		prefix += "/"
	}
	if !strings.HasPrefix(candidate, prefix) {
		return false
	}
	return !strings.Contains(strings.TrimPrefix(candidate, prefix), "/")
}
