package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/example/safefilehub/internal/auth"
	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
)

type listFilesRepo struct {
	root  db.StorageRoot
	files []db.File
}

func (r listFilesRepo) StorageRootByID(context.Context, int64) (db.StorageRoot, error) {
	return r.root, nil
}
func (r listFilesRepo) FilesUnderRoot(context.Context, int64) ([]db.File, error) { return r.files, nil }

type listAuthorizer struct{ allowed map[string]bool }

func (a listAuthorizer) Authorize(_ context.Context, _ int64, _ int64, path, action string) (bool, error) {
	return action == "read" && a.allowed[path], nil
}

func TestListFilesRequiresSessionAndFiltersChildren(t *testing.T) {
	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{TTL: time.Hour})
	defer sessions.Close()
	rootDir := t.TempDir()
	repo := listFilesRepo{root: db.StorageRoot{ID: 3, Path: rootDir}, files: []db.File{
		{LogicalPath: "/docs/allowed.txt", ObjectKey: "objects/a", Size: 4, MD5Status: db.MD5Ready, MD5Digest: "d41d8cd98f00b204e9800998ecf8427e"},
		{LogicalPath: "/docs/secret.txt", ObjectKey: "objects/b", Size: 6},
		{LogicalPath: "/docs/.hidden", ObjectKey: "objects/c", Size: 1},
		{LogicalPath: "/docs/staged.txt", ObjectKey: "staging/partial", Size: 2},
		{LogicalPath: "/other/nope.txt", ObjectKey: "objects/d", Size: 5},
	}}
	h, err := NewServerWithFiles(config.Default(), rejectingAuthenticator{}, sessions, repo, listAuthorizer{allowed: map[string]bool{"/docs": true, "/docs/allowed.txt": true}})
	if err != nil {
		t.Fatal(err)
	}

	unauthorized := httptest.NewRecorder()
	h.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/roots/3/files?path=docs", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", unauthorized.Code)
	}

	id, err := sessions.Create(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/roots/3/files?path=docs", nil)
	req.AddCookie(&http.Cookie{Name: sessions.CookieName(), Value: id})
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if contains := rec.Body.String(); contains == "" || stringContainsAny(contains, rootDir, "secret.txt", ".hidden", "staged.txt", "objects/") {
		t.Fatalf("listing leaked or included disallowed data: %s", contains)
	}
	var response fileListResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Path != "/docs" || len(response.Files) != 1 || response.Files[0].Name != "allowed.txt" || response.Files[0].Path != "/docs/allowed.txt" {
		t.Fatalf("response = %#v", response)
	}
	if got := response.Files[0]; got.MD5Status != db.MD5Ready || got.MD5Digest != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Fatalf("md5 response = %#v", got)
	}
}

func TestListFilesNeverExposesMD5ErrorOrInvalidDigest(t *testing.T) {
	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{TTL: time.Hour})
	defer sessions.Close()
	repo := listFilesRepo{root: db.StorageRoot{ID: 3, Path: t.TempDir()}, files: []db.File{{LogicalPath: "/docs/bad.txt", ObjectKey: "objects/a", Size: 4, MD5Status: db.MD5Failed, MD5Digest: "not-a-digest", MD5Error: "open /private/objects/a: permission denied"}}}
	h, err := NewServerWithFiles(config.Default(), rejectingAuthenticator{}, sessions, repo, listAuthorizer{allowed: map[string]bool{"/docs": true, "/docs/bad.txt": true}})
	if err != nil {
		t.Fatal(err)
	}
	id, err := sessions.Create(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/roots/3/files?path=docs", nil)
	req.AddCookie(&http.Cookie{Name: sessions.CookieName(), Value: id})
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if stringContainsAny(rec.Body.String(), "MD5Error", "/private/", "permission denied", "not-a-digest") {
		t.Fatalf("unsafe MD5 data leaked: %s", rec.Body.String())
	}
	var response fileListResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if got := response.Files[0]; got.MD5Status != db.MD5Failed || got.MD5Digest != "" {
		t.Fatalf("md5 response = %#v", got)
	}
}

func TestListFilesRejectsInvalidPathAndUnauthorizedDirectory(t *testing.T) {
	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{TTL: time.Hour})
	defer sessions.Close()
	h, err := NewServerWithFiles(config.Default(), rejectingAuthenticator{}, sessions, listFilesRepo{root: db.StorageRoot{ID: 3, Path: t.TempDir()}}, listAuthorizer{})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := sessions.Create(context.Background(), 7)
	for _, target := range []string{"/roots/3/files?path=..", "/roots/3/files?path=docs"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.AddCookie(&http.Cookie{Name: sessions.CookieName(), Value: id})
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status %d", target, rec.Code)
		}
	}
}

func stringContainsAny(s string, values ...string) bool {
	for _, v := range values {
		if v != "" && len(s) >= len(v) {
			for i := 0; i+len(v) <= len(s); i++ {
				if s[i:i+len(v)] == v {
					return true
				}
			}
		}
	}
	return false
}
