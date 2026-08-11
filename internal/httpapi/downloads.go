package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/download"
	"github.com/example/safefilehub/internal/limits"
	"github.com/example/safefilehub/internal/metrics"
	"github.com/example/safefilehub/internal/storage"
)

type downloadRepository interface {
	StorageRootByID(context.Context, int64) (db.StorageRoot, error)
	FileByID(context.Context, int64) (db.File, error)
}
type downloadAuthorizer interface {
	Authorize(context.Context, int64, int64, string, string) (bool, error)
}

// NewServerWithDownloads composes the existing authenticated listing surface
// with a single-object download route. Multi-range requests are deliberately
// rejected (416), avoiding multipart buffering and ambiguity.
func NewServerWithDownloads(cfg config.Config, users authenticator, sessions sessionManager, repo downloadRepository, authorizer downloadAuthorizer, store *storage.ObjectStore) (http.Handler, error) {
	return newServerWithDownloads(cfg, users, sessions, repo, authorizer, store, nil)
}

// NewServerWithDownloadsAndObservability preserves NewServerWithDownloads's
// API and uses one caller-owned Metrics instance for the composed service.
func NewServerWithDownloadsAndObservability(cfg config.Config, users authenticator, sessions sessionManager, repo downloadRepository, authorizer downloadAuthorizer, store *storage.ObjectStore, observability *metrics.Metrics) (http.Handler, error) {
	if observability == nil {
		return nil, errors.New("observability dependencies are required")
	}
	return newServerWithDownloads(cfg, users, sessions, repo, authorizer, store, observability)
}

func newServerWithDownloads(cfg config.Config, users authenticator, sessions sessionManager, repo downloadRepository, authorizer downloadAuthorizer, store *storage.ObjectStore, observability *metrics.Metrics) (http.Handler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if users == nil || sessions == nil || repo == nil || authorizer == nil || store == nil {
		return nil, errors.New("download dependencies are required")
	}
	limiter, err := limits.NewDownloadLimiter(cfg.DownloadConcurrency)
	if err != nil {
		return nil, err
	}
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", healthz)
	m.HandleFunc("POST /login", login(users, sessions))
	m.HandleFunc("POST /logout", logout(sessions))
	m.Handle("GET /session", requireSession(sessions, http.HandlerFunc(sessionStatus)))
	download := http.Handler(http.HandlerFunc(downloadFile(repo, authorizer, store)))
	if observability != nil {
		download = limitDownloadWithMetrics(limiter, observability, observeDownload(observability, download))
	} else {
		download = limitDownload(limiter, download)
	}
	downloadHandler := requireSession(sessions, download)
	m.Handle("GET /api/files/{fileID}", downloadHandler)
	m.Handle("HEAD /api/files/{fileID}", downloadHandler)
	if observability != nil {
		return observedHandler(cfg, m, observability), nil
	}
	return RequestLimits(cfg, m), nil
}

func limitDownload(limiter *limits.DownloadLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lease, err := limiter.TryAcquire()
		if err != nil {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "download capacity exceeded", http.StatusTooManyRequests)
			return
		}
		defer lease.Release()
		next.ServeHTTP(w, r)
	})
}

func downloadFile(repo downloadRepository, authorizer downloadAuthorizer, store *storage.ObjectStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("fileID"), 10, 64)
		if err != nil || id <= 0 {
			http.NotFound(w, r)
			return
		}
		uid, ok := r.Context().Value(sessionUserIDKey{}).(int64)
		if !ok || uid <= 0 {
			http.NotFound(w, r)
			return
		}
		file, err := repo.FileByID(r.Context(), id)
		if err != nil {
			hiddenDownloadError(w, r, err)
			return
		}
		allowed, err := authorizer.Authorize(r.Context(), uid, file.RootID, file.LogicalPath, "read")
		if err != nil {
			http.Error(w, "authorize download", http.StatusInternalServerError)
			return
		}
		if !allowed {
			http.NotFound(w, r)
			return
		}
		// Root lookup validates that its metadata still exists. Object opening is
		// descriptor-relative and O_NOFOLLOW inside ObjectStore.
		if _, err := repo.StorageRootByID(r.Context(), file.RootID); err != nil {
			hiddenDownloadError(w, r, err)
			return
		}
		object, err := store.Open(file.ObjectKey)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer object.Close()
		info, err := object.Stat()
		if err != nil || !info.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		size := info.Size()
		if file.Size != size {
			http.NotFound(w, r)
			return
		}
		etag := downloadETag(file)
		mod := file.UpdatedAt.UTC()
		if mod.IsZero() {
			mod = info.ModTime().UTC()
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", mod.Format(http.TimeFormat))
		w.Header().Set("Content-Disposition", contentDisposition(file.LogicalPath))
		start, length, status := int64(0), size, http.StatusOK
		if raw := r.Header.Get("Range"); raw != "" && ifRangeMatches(r.Header.Get("If-Range"), etag, mod) {
			rng, err := download.ParseRange(raw, size)
			if err != nil {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			start, length, status = rng.Start, rng.Length, http.StatusPartialContent
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, size))
		}
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		if r.Method == http.MethodHead {
			w.WriteHeader(status)
			return
		}
		if _, err := object.Seek(start, io.SeekStart); err != nil {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = io.CopyBuffer(w, io.LimitReader(object, length), make([]byte, 128*1024))
	}
}
func hiddenDownloadError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, db.ErrNotFound) {
		http.NotFound(w, r)
	} else {
		http.Error(w, "load download", http.StatusInternalServerError)
	}
}
func downloadETag(f db.File) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d", f.ObjectKey, f.Size, f.UpdatedAt.UTC().UnixNano())))
	return `"` + hex.EncodeToString(sum[:]) + `"`
}
func ifRangeMatches(v, etag string, mod time.Time) bool {
	if v == "" {
		return true
	}
	if strings.TrimSpace(v) == etag {
		return true
	}
	t, err := http.ParseTime(v)
	return err == nil && !mod.After(t)
}
func contentDisposition(logical string) string {
	name := logical[strings.LastIndex(logical, "/")+1:]
	ascii := strings.Map(func(r rune) rune {
		if r >= 0x20 && r <= 0x7e && r != '"' && r != '\\' {
			return r
		}
		return '_'
	}, name)
	if ascii == "" {
		ascii = "download"
	}
	return `attachment; filename="` + ascii + `"; filename*=UTF-8''` + url.PathEscape(name)
}
