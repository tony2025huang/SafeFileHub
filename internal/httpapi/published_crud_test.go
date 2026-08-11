package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
	appLog "github.com/example/safefilehub/internal/observability"
	"github.com/example/safefilehub/internal/permission"
	"github.com/example/safefilehub/internal/storage"
)

type publishedFixture struct {
	repo          *db.Repository
	root          db.StorageRoot
	owner, denied db.User
	store         *storage.ObjectStore
	h             http.Handler
	logs          *bytes.Buffer
}

func newPublishedFixture(t *testing.T) publishedFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	repo, err := db.Open(ctx, filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	owner, _ := repo.CreateUser(ctx, db.User{Username: "owner", PasswordHash: "x"})
	denied, _ := repo.CreateUser(ctx, db.User{Username: "denied", PasswordHash: "x"})
	root, _ := repo.CreateStorageRoot(ctx, db.StorageRoot{Name: "root", Path: dir})
	for _, action := range []string{"write", "delete"} {
		if _, err := repo.CreatePermission(ctx, db.Permission{UserID: owner.ID, RootID: root.ID, PathPrefix: "/", Action: action, Allow: true}); err != nil {
			t.Fatal(err)
		}
	}
	store, err := storage.NewObjectStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	a := permission.NewAuthorizer(repo, config.Default().NamePolicy)
	mux := http.NewServeMux()
	mux.Handle("POST /api/files", http.HandlerFunc(createEmptyFile(repo, a, config.Default().NamePolicy, store)))
	mux.Handle("POST /api/directories", http.HandlerFunc(createDirectory(repo, a, config.Default().NamePolicy)))
	mux.Handle("DELETE /api/files/{fileID}", http.HandlerFunc(deleteFile(repo, a, store)))
	mux.Handle("DELETE /api/directories/{directoryID}", http.HandlerFunc(deleteDirectory(repo, a)))
	logs := &bytes.Buffer{}
	h := requestContext(config.Default(), applicationLogWithLogger(appLog.NewMulti(appLog.New(logs, appLog.FormatJSON)), mux))
	return publishedFixture{repo, root, owner, denied, store, h, logs}
}
func (f publishedFixture) call(t *testing.T, method, target, body string, user int64) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.RemoteAddr = "198.51.100.8:2222"
	ctx := context.WithValue(r.Context(), sessionUserIDKey{}, user)
	ctx = context.WithValue(ctx, sessionAuditIDKey{}, "audit-test")
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}
func (f publishedFixture) logsFor(t *testing.T) []appLog.Event {
	t.Helper()
	var out []appLog.Event
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		var e appLog.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}
func assertPublishedAudit(t *testing.T, f publishedFixture, action string, status int) {
	t.Helper()
	var event *appLog.Event
	for i := range f.logsFor(t) {
		e := f.logsFor(t)[i]
		if e.Operation == action+".request" && e.Status == status {
			event = &e
		}
	}
	if event == nil || event.Success != (status < 400) || event.ClientIP != "198.51.100.8" || event.PeerIP != "198.51.100.8" || event.RequestID == "" || event.SessionAuditID != "audit-test" {
		t.Fatalf("request event=%#v", event)
	}
	audits, err := f.repo.AuditEventsForUser(context.Background(), f.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, a := range audits {
		if a.Action == action && a.Status == status {
			found = true
			if !strings.Contains(a.Detail, "result=") || strings.Contains(strings.ToLower(a.Detail), "password=") {
				t.Fatalf("audit detail=%q", a.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("persistent audit %s status=%d absent: %#v", action, status, audits)
	}
}
func TestPublishedCRUDAcceptance(t *testing.T) {
	f := newPublishedFixture(t)
	if got := f.call(t, "POST", "/api/directories", `{"root_id":1,"path":"docs"}`, f.owner.ID).Code; got != 201 {
		t.Fatal(got)
	}
	assertPublishedAudit(t, f, "directory.create", 201)
	if got := f.call(t, "POST", "/api/files", `{"root_id":1,"path":"docs/empty"}`, f.owner.ID).Code; got != 201 {
		t.Fatal(got)
	}
	assertPublishedAudit(t, f, "file.create", 201)
	if got := f.call(t, "POST", "/api/files", `{"root_id":1,"path":"docs/empty"}`, f.owner.ID).Code; got != 409 {
		t.Fatal(got)
	}
	assertPublishedAudit(t, f, "file.create", 409)
	if got := f.call(t, "POST", "/api/directories", `{"root_id":1,"path":"docs/empty"}`, f.owner.ID).Code; got != 409 {
		t.Fatal(got)
	}
	assertPublishedAudit(t, f, "directory.create", 409)
	if got := f.call(t, "POST", "/api/files", `{"root_id":1,"path":"../bad"}`, f.owner.ID).Code; got != 400 {
		t.Fatal(got)
	}
	assertPublishedAudit(t, f, "file.create", 400)
	if got := f.call(t, "POST", "/api/directories", `{"root_id":1,"path":"missing/child"}`, f.owner.ID).Code; got != 404 {
		t.Fatal(got)
	}
	assertPublishedAudit(t, f, "directory.create", 404)
	if got := f.call(t, "POST", "/api/directories", `{"root_id":1,"path":"../bad"}`, f.owner.ID).Code; got != 400 {
		t.Fatal(got)
	}
	assertPublishedAudit(t, f, "directory.create", 400)
	if got := f.call(t, "POST", "/api/files", `{"root_id":1,"path":"denied"}`, f.denied.ID).Code; got != 403 {
		t.Fatal(got)
	}
	file, err := f.repo.FileByRootAndPath(context.Background(), f.root.ID, "/docs/empty")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.call(t, "DELETE", "/api/files/"+itoa(file.ID), "", f.denied.ID).Code; got != 403 {
		t.Fatal(got)
	}
	if got := f.call(t, "DELETE", "/api/files/999999", "", f.owner.ID).Code; got != 404 {
		t.Fatal(got)
	}
	if got := f.call(t, "DELETE", "/api/files/"+itoa(file.ID), "", f.owner.ID).Code; got != 204 {
		t.Fatal(got)
	}
	assertPublishedAudit(t, f, "file.delete", 204)
	d, err := f.repo.DirectoryByRootAndPath(context.Background(), f.root.ID, "/docs")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.call(t, "DELETE", "/api/directories/"+itoa(d.ID), "", f.owner.ID).Code; got != 204 {
		t.Fatal(got)
	}
	assertPublishedAudit(t, f, "directory.delete", 204)
}

func TestDeleteFileObjectFailureLeavesDurableTombstone(t *testing.T) {
	f := newPublishedFixture(t)
	key, err := f.store.CreateEmpty("/pending")
	if err != nil {
		t.Fatal(err)
	}
	file, err := f.repo.CreateFile(context.Background(), db.File{RootID: f.root.ID, LogicalPath: "/pending", ObjectKey: key, CreatedByUserID: f.owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	if got := f.call(t, "DELETE", "/api/files/"+itoa(file.ID), "", f.owner.ID).Code; got != http.StatusInternalServerError {
		t.Fatalf("delete=%d", got)
	}
	assertPublishedAudit(t, f, "file.delete", http.StatusInternalServerError)
	if _, err := f.repo.FileByID(context.Background(), file.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("active tombstone visible: %v", err)
	}
	if _, err := f.repo.FileForDeletion(context.Background(), file.ID); err != nil {
		t.Fatalf("tombstone missing: %v", err)
	}
}

func TestDeleteDirectoryRejectsNonEmpty(t *testing.T) {
	f := newPublishedFixture(t)
	if got := f.call(t, "POST", "/api/directories", `{"root_id":1,"path":"parent"}`, f.owner.ID).Code; got != http.StatusCreated {
		t.Fatal(got)
	}
	d, err := f.repo.DirectoryByRootAndPath(context.Background(), f.root.ID, "/parent")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.call(t, "POST", "/api/files", `{"root_id":1,"path":"parent/child"}`, f.owner.ID).Code; got != http.StatusCreated {
		t.Fatal(got)
	}
	if got := f.call(t, "DELETE", "/api/directories/"+itoa(d.ID), "", f.owner.ID).Code; got != http.StatusConflict {
		t.Fatalf("nonrecursive delete=%d", got)
	}
	assertPublishedAudit(t, f, "directory.delete", http.StatusConflict)
}
