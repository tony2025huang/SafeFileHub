package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/example/safefilehub/internal/archive"
	"github.com/example/safefilehub/internal/auth"
	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
)

type archiveRepo struct{ files []db.File }

func (r archiveRepo) StorageRootByID(context.Context, int64) (db.StorageRoot, error) {
	return db.StorageRoot{ID: 1}, nil
}
func (r archiveRepo) FilesUnderRoot(context.Context, int64) ([]db.File, error) { return r.files, nil }

type archiveHTTPAuth struct{ denied string }

func (a archiveHTTPAuth) Authorize(_ context.Context, _, _ int64, p, action string) (bool, error) {
	return action == "archive" && p != a.denied, nil
}

type archiveSource map[string]string

func (s archiveSource) Open(_ context.Context, k string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(s[k])), nil
}

func TestArchiveHTTPChecksEachPathAndNeverListsArtifacts(t *testing.T) {
	m, err := archive.New(archive.Options{Workers: 1, MaxFiles: 4, MaxBytes: 20, TTL: time.Hour, TempDir: t.TempDir()}, archiveSource{"a": "a", "b": "b"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{TTL: time.Hour})
	defer sessions.Close()
	sid, _ := sessions.Create(context.Background(), 7)
	repo := archiveRepo{files: []db.File{{ID: 1, RootID: 1, LogicalPath: "/d/a.txt", ObjectKey: "a", Size: 1}, {ID: 2, RootID: 1, LogicalPath: "/d/private.txt", ObjectKey: "b", Size: 1}}}
	h, err := NewServerWithArchives(config.Default(), rejectingAuthenticator{}, sessions, repo, archiveHTTPAuth{denied: "/d/private.txt"}, m)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/roots/1/archives", bytes.NewBufferString(`{"path":"/d"}`))
	req.AddCookie(&http.Cookie{Name: sessions.CookieName(), Value: sid})
	q := httptest.NewRecorder()
	h.ServeHTTP(q, req)
	if q.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", q.Code, q.Body.String())
	}
	// Archive artifacts live only in the archive manager and never enter FilesUnderRoot/listing rows.
	if got, _ := repo.FilesUnderRoot(context.Background(), 1); len(got) != 2 {
		t.Fatalf("listing changed: %#v", got)
	}
}
func TestArchiveHTTPCreatesCancelsAndDownloads(t *testing.T) {
	m, err := archive.New(archive.Options{Workers: 1, MaxFiles: 4, MaxBytes: 20, TTL: time.Hour, TempDir: t.TempDir()}, archiveSource{"a": "a"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{TTL: time.Hour})
	defer sessions.Close()
	sid, _ := sessions.Create(context.Background(), 7)
	h, err := NewServerWithArchives(config.Default(), rejectingAuthenticator{}, sessions, archiveRepo{files: []db.File{{ID: 1, RootID: 1, LogicalPath: "/d/a.txt", ObjectKey: "a", Size: 1}}}, archiveHTTPAuth{}, m)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/roots/1/archives", bytes.NewBufferString(`{"path":"/d"}`))
	req.AddCookie(&http.Cookie{Name: sessions.CookieName(), Value: sid})
	q := httptest.NewRecorder()
	h.ServeHTTP(q, req)
	if q.Code != http.StatusAccepted {
		t.Fatalf("create=%d %s", q.Code, q.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(q.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		r := httptest.NewRequest(http.MethodGet, "/api/archives/"+out.ID, nil)
		r.AddCookie(&http.Cookie{Name: sessions.CookieName(), Value: sid})
		q = httptest.NewRecorder()
		h.ServeHTTP(q, r)
		if q.Code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not downloadable: %d", q.Code)
		}
		time.Sleep(time.Millisecond)
	}
	r := httptest.NewRequest(http.MethodDelete, "/api/archives/"+out.ID, nil)
	r.AddCookie(&http.Cookie{Name: sessions.CookieName(), Value: sid})
	q = httptest.NewRecorder()
	h.ServeHTTP(q, r)
	if q.Code != http.StatusNoContent {
		t.Fatalf("cancel=%d", q.Code)
	}
}
