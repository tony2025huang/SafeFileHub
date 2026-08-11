package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/example/safefilehub/internal/auth"
	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/permission"
	"github.com/example/safefilehub/internal/storage"
	"github.com/example/safefilehub/internal/upload"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUploadLifecycle(t *testing.T) {
	d := t.TempDir()
	repo, err := db.Open(context.Background(), d+"/db")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	u, _ := repo.CreateUser(context.Background(), db.User{Username: "u", PasswordHash: "x"})
	root, _ := repo.CreateStorageRoot(context.Background(), db.StorageRoot{Name: "r", Path: d})
	_, _ = repo.CreatePermission(context.Background(), db.Permission{UserID: u.ID, RootID: root.ID, PathPrefix: "/", Action: "write", Allow: true})
	store, err := storage.NewObjectStore(d)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{TTL: time.Hour})
	defer sessions.Close()
	sid, _ := sessions.Create(context.Background(), u.ID)
	h, err := NewServerWithUploads(config.Default(), rejectingAuthenticator{}, sessions, repo, permission.NewAuthorizer(repo, config.Default().NamePolicy), store)
	if err != nil {
		t.Fatal(err)
	}
	post := httptest.NewRequest(http.MethodPost, "/api/uploads", bytes.NewBufferString(`{"root_id":1,"path":"docs/a.txt","size":5}`))
	post.RemoteAddr = "192.0.2.1:1"
	post.AddCookie(&http.Cookie{Name: sessions.CookieName(), Value: sid})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, post)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create %d %s", rr.Code, rr.Body.String())
	}
	var out uploadResponse
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.UploadID == "" || out.Offset != 0 || out.ChunkSize <= 0 {
		t.Fatalf("response %#v", out)
	}
	head := httptest.NewRequest(http.MethodHead, "/api/uploads/"+out.UploadID, nil)
	head.AddCookie(&http.Cookie{Name: sessions.CookieName(), Value: sid})
	hr := httptest.NewRecorder()
	h.ServeHTTP(hr, head)
	if hr.Code != 200 || hr.Header().Get("Upload-Length") != "5" {
		t.Fatalf("head %d %#v", hr.Code, hr.Header())
	}
	patch := httptest.NewRequest(http.MethodPatch, "/api/uploads/"+out.UploadID, bytes.NewBufferString("hello"))
	patch.Header.Set("Content-Type", "application/offset+octet-stream")
	patch.Header.Set("Upload-Offset", "0")
	patch.RemoteAddr = "192.0.2.1:1"
	patch.AddCookie(&http.Cookie{Name: sessions.CookieName(), Value: sid})
	pr := httptest.NewRecorder()
	h.ServeHTTP(pr, patch)
	if pr.Code != http.StatusNoContent || pr.Header().Get("Upload-Offset") != "5" {
		t.Fatalf("patch %d %#v", pr.Code, pr.Header())
	}
	bad := httptest.NewRequest(http.MethodPatch, "/api/uploads/"+out.UploadID, bytes.NewBufferString("x"))
	bad.Header.Set("Content-Type", "application/offset+octet-stream")
	bad.Header.Set("Upload-Offset", "0")
	bad.RemoteAddr = "192.0.2.1:1"
	bad.AddCookie(&http.Cookie{Name: sessions.CookieName(), Value: sid})
	br := httptest.NewRecorder()
	h.ServeHTTP(br, bad)
	if br.Code != http.StatusConflict {
		t.Fatalf("wrong offset %d", br.Code)
	}
	del := httptest.NewRequest(http.MethodDelete, "/api/uploads/"+out.UploadID, nil)
	del.AddCookie(&http.Cookie{Name: sessions.CookieName(), Value: sid})
	dr := httptest.NewRecorder()
	h.ServeHTTP(dr, del)
	if dr.Code != 204 {
		t.Fatalf("delete %d", dr.Code)
	}
}

func TestCreateUploadDecodesBrowserEscapedLogicalPath(t *testing.T) {
	d := t.TempDir()
	repo, err := db.Open(context.Background(), d+"/db")
	if err != nil { t.Fatal(err) }
	defer repo.Close()
	u, _ := repo.CreateUser(context.Background(), db.User{Username: "u", PasswordHash: "x"})
	root, _ := repo.CreateStorageRoot(context.Background(), db.StorageRoot{Name: "r", Path: d})
	_, _ = repo.CreatePermission(context.Background(), db.Permission{UserID: u.ID, RootID: root.ID, PathPrefix: "/", Action: "write", Allow: true})
	store, _ := storage.NewObjectStore(d); defer store.Close()
	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{TTL: time.Hour}); defer sessions.Close()
	sid, _ := sessions.Create(context.Background(), u.ID)
	h, err := NewServerWithUploads(config.Default(), rejectingAuthenticator{}, sessions, repo, permission.NewAuthorizer(repo, config.Default().NamePolicy), store)
	if err != nil { t.Fatal(err) }
	body := `{"root_id":1,"path":"drop/a%2Bb/%25%3F%20%E7%A9%BA/%E4%B8%AD%E6%96%87%F0%9F%98%80.txt","size":1}`
	req := httptest.NewRequest(http.MethodPost, "/api/uploads", bytes.NewBufferString(body)); req.AddCookie(&http.Cookie{Name: sessions.CookieName(), Value: sid})
	rr := httptest.NewRecorder(); h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated { t.Fatalf("create = %d: %s", rr.Code, rr.Body.String()) }
	var out uploadResponse; if err := json.NewDecoder(rr.Body).Decode(&out); err != nil { t.Fatal(err) }
	s, err := repo.UploadSessionByID(context.Background(), out.UploadID); if err != nil { t.Fatal(err) }
	if got, want := s.LogicalPath, "/drop/a+b/%? 空/中文😀.txt"; got != want { t.Fatalf("logical path = %q, want %q", got, want) }
}

func TestUploadSessionAuthorizationUsesOperationPermission(t *testing.T) {
	d := t.TempDir()
	repo, err := db.Open(context.Background(), d+"/db")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	owner, _ := repo.CreateUser(context.Background(), db.User{Username: "owner", PasswordHash: "x"})
	readOnly, _ := repo.CreateUser(context.Background(), db.User{Username: "read", PasswordHash: "x"})
	writeOnly, _ := repo.CreateUser(context.Background(), db.User{Username: "write", PasswordHash: "x"})
	deleteOnly, _ := repo.CreateUser(context.Background(), db.User{Username: "delete", PasswordHash: "x"})
	root, _ := repo.CreateStorageRoot(context.Background(), db.StorageRoot{Name: "r", Path: d})
	for _, p := range []db.Permission{{UserID: readOnly.ID, RootID: root.ID, PathPrefix: "/", Action: "read", Allow: true}, {UserID: writeOnly.ID, RootID: root.ID, PathPrefix: "/", Action: "write", Allow: true}, {UserID: deleteOnly.ID, RootID: root.ID, PathPrefix: "/", Action: "delete", Allow: true}} {
		_, _ = repo.CreatePermission(context.Background(), p)
	}
	store, _ := storage.NewObjectStore(d)
	defer store.Close()
	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{TTL: time.Hour})
	defer sessions.Close()
	h, err := NewServerWithUploads(config.Default(), rejectingAuthenticator{}, sessions, repo, permission.NewAuthorizer(repo, config.Default().NamePolicy), store)
	if err != nil {
		t.Fatal(err)
	}
	m := upload.New(repo, store, config.Default().ChunkSize, time.Hour)
	s, err := m.Create(context.Background(), owner.ID, root.ID, "/a", 1)
	if err != nil {
		t.Fatal(err)
	}
	request := func(user int64, method string) int {
		sid, _ := sessions.Create(context.Background(), user)
		req := httptest.NewRequest(method, "/api/uploads/"+s.ID, bytes.NewBufferString("x"))
		req.AddCookie(&http.Cookie{Name: sessions.CookieName(), Value: sid})
		if method == http.MethodPatch {
			req.Header.Set("Content-Type", "application/offset+octet-stream")
			req.Header.Set("Upload-Offset", "0")
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	if got := request(readOnly.ID, http.MethodHead); got != 200 {
		t.Fatalf("read HEAD = %d", got)
	}
	if got := request(readOnly.ID, http.MethodPatch); got != 404 {
		t.Fatalf("read PATCH = %d", got)
	}
	if got := request(writeOnly.ID, http.MethodHead); got != 404 {
		t.Fatalf("write HEAD = %d", got)
	}
	if got := request(writeOnly.ID, http.MethodPatch); got != 204 {
		t.Fatalf("write PATCH = %d", got)
	}
	if got := request(deleteOnly.ID, http.MethodHead); got != 404 {
		t.Fatalf("delete HEAD = %d", got)
	}
	if got := request(deleteOnly.ID, http.MethodDelete); got != 204 {
		t.Fatalf("delete DELETE = %d", got)
	}
}

func TestNewServerWithUploadsRetainsFileListingRoute(t *testing.T) {
	d := t.TempDir()
	repo, err := db.Open(context.Background(), d+"/db")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	u, _ := repo.CreateUser(context.Background(), db.User{Username: "u", PasswordHash: "x"})
	root, _ := repo.CreateStorageRoot(context.Background(), db.StorageRoot{Name: "r", Path: d})
	_, _ = repo.CreatePermission(context.Background(), db.Permission{UserID: u.ID, RootID: root.ID, PathPrefix: "/", Action: "read", Allow: true})
	store, _ := storage.NewObjectStore(d)
	defer store.Close()
	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{TTL: time.Hour})
	defer sessions.Close()
	sid, _ := sessions.Create(context.Background(), u.ID)
	h, err := NewServerWithUploads(config.Default(), rejectingAuthenticator{}, sessions, repo, permission.NewAuthorizer(repo, config.Default().NamePolicy), store)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/roots/1/files?path=/", nil)
	req.AddCookie(&http.Cookie{Name: sessions.CookieName(), Value: sid})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("listing route = %d: %s", rr.Code, rr.Body.String())
	}
}
