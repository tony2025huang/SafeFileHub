package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/example/safefilehub/internal/auth"
	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/storage"
)

type downloadRepo struct {
	root db.StorageRoot
	file db.File
	err  error
}

func (r downloadRepo) StorageRootByID(context.Context, int64) (db.StorageRoot, error) {
	return r.root, r.err
}
func (r downloadRepo) FileByID(context.Context, int64) (db.File, error) { return r.file, r.err }

type downloadAuth struct{ allow bool }

func (a downloadAuth) Authorize(context.Context, int64, int64, string, string) (bool, error) {
	return a.allow, nil
}

func TestDownloadRangeETagHeadAndHidden(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.NewObjectStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key, w, err := store.Create("/报告 🐱.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write([]byte("abcdefghij")); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{TTL: time.Hour})
	defer sessions.Close()
	sid, _ := sessions.Create(context.Background(), 7)
	repo := downloadRepo{root: db.StorageRoot{ID: 3, Path: dir}, file: db.File{ID: 9, RootID: 3, LogicalPath: "/报告 🐱.txt", ObjectKey: key, Size: 10, UpdatedAt: time.Unix(100, 0)}}
	h, err := NewServerWithDownloads(config.Default(), rejectingAuthenticator{}, sessions, repo, downloadAuth{true}, store)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, rangeHeader, ifRange string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/files/9", nil)
		r.AddCookie(&http.Cookie{Name: sessions.CookieName(), Value: sid})
		r.Header.Set("Range", rangeHeader)
		r.Header.Set("If-Range", ifRange)
		q := httptest.NewRecorder()
		h.ServeHTTP(q, r)
		return q
	}
	full := request("GET", "", "")
	if full.Code != 200 || full.Body.String() != "abcdefghij" || full.Header().Get("Accept-Ranges") != "bytes" || full.Header().Get("Content-Length") != "10" {
		t.Fatalf("full: %d %q %#v", full.Code, full.Body.String(), full.Header())
	}
	etag := full.Header().Get("Etag")
	if etag == "" || full.Header().Get("Last-Modified") == "" || full.Header().Get("Content-Disposition") == "" {
		t.Fatalf("missing metadata %#v", full.Header())
	}
	partial := request("GET", "bytes=2-5", etag)
	if partial.Code != 206 || partial.Body.String() != "cdef" || partial.Header().Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("partial: %d %q %#v", partial.Code, partial.Body.String(), partial.Header())
	}
	stale := request("GET", "bytes=2-5", "\"stale\"")
	if stale.Code != 200 || stale.Body.String() != "abcdefghij" {
		t.Fatalf("stale: %d %q", stale.Code, stale.Body.String())
	}
	invalid := request("GET", "bytes=0-1,3-4", "")
	if invalid.Code != 416 || invalid.Header().Get("Content-Range") != "bytes */10" {
		t.Fatalf("invalid: %d %#v", invalid.Code, invalid.Header())
	}
	head := request("HEAD", "", "")
	if head.Code != 200 || head.Body.Len() != 0 {
		t.Fatalf("head: %d %q", head.Code, head.Body.String())
	}
	denied, err := NewServerWithDownloads(config.Default(), rejectingAuthenticator{}, sessions, repo, downloadAuth{}, store)
	if err != nil {
		t.Fatal(err)
	}
	q := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/files/9", bytes.NewReader(nil))
	r.AddCookie(&http.Cookie{Name: sessions.CookieName(), Value: sid})
	denied.ServeHTTP(q, r)
	if q.Code != http.StatusNotFound {
		t.Fatalf("denied=%d", q.Code)
	}
}
