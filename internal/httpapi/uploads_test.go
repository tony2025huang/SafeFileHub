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
