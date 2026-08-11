package httpapi

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/permission"
	"github.com/example/safefilehub/internal/storage"
)

// failingCreateRepository makes the metadata half of publication fail after a
// real object has been allocated, exercising the rollback boundary.
type failingCreateRepository struct {
	*db.Repository
	key string
}

func (r *failingCreateRepository) CreateFile(_ context.Context, f db.File) (db.File, error) {
	r.key = f.ObjectKey
	return db.File{}, errors.New("database unavailable")
}

func TestCreateEmptyFileValidationAuthorizationAndMetadataRollback(t *testing.T) {
	d := t.TempDir()
	real, err := db.Open(context.Background(), d+"/db")
	if err != nil {
		t.Fatal(err)
	}
	defer real.Close()
	u, _ := real.CreateUser(context.Background(), db.User{Username: "writer", PasswordHash: "x"})
	root, _ := real.CreateStorageRoot(context.Background(), db.StorageRoot{Name: "root", Path: d})
	_, _ = real.CreatePermission(context.Background(), db.Permission{UserID: u.ID, RootID: root.ID, PathPrefix: "/", Action: "write", Allow: true})
	store, _ := storage.NewObjectStore(d)
	defer store.Close()
	authorizer := permission.NewAuthorizer(real, config.Default().NamePolicy)
	success := createEmptyFile(real, authorizer, config.Default().NamePolicy, store)
	callSuccess := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString(body))
		r = r.WithContext(context.WithValue(r.Context(), sessionUserIDKey{}, u.ID))
		w := httptest.NewRecorder()
		success.ServeHTTP(w, r)
		return w
	}
	if got := callSuccess(`{"root_id":1,"path":"empty.txt"}`).Code; got != http.StatusCreated {
		t.Fatalf("success=%d", got)
	}
	f, err := real.FileByRootAndPath(context.Background(), root.ID, "/empty.txt")
	if err != nil || f.Size != 0 {
		t.Fatalf("metadata=%#v err=%v", f, err)
	}
	object, err := store.Open(f.ObjectKey)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := object.Stat()
	_ = object.Close()
	if info.Size() != 0 {
		t.Fatalf("object size=%d", info.Size())
	}
	if got := callSuccess(`{"root_id":1,"path":"empty.txt"}`).Code; got != http.StatusConflict {
		t.Fatalf("conflict=%d", got)
	}
	failing := &failingCreateRepository{Repository: real}
	h := createEmptyFile(failing, authorizer, config.Default().NamePolicy, store)
	call := func(body string, userID int64) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/files", bytes.NewBufferString(body))
		r = r.WithContext(context.WithValue(r.Context(), sessionUserIDKey{}, userID))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if got := call(`{"root_id":1,"path":"../secret"}`, u.ID).Code; got != http.StatusBadRequest {
		t.Fatalf("traversal=%d", got)
	}
	if got := call(`{"root_id":1,"path":"x"}`, 999).Code; got != http.StatusForbidden {
		t.Fatalf("permission=%d", got)
	}
	if got := call(`{"root_id":1,"path":"missing/x"}`, u.ID).Code; got != http.StatusNotFound {
		t.Fatalf("parent=%d", got)
	}
	if got := call(`{"root_id":1,"path":"rollback.txt"}`, u.ID).Code; got != http.StatusInternalServerError {
		t.Fatalf("metadata failure=%d", got)
	}
	if failing.key == "" {
		t.Fatal("CreateFile did not receive opaque key")
	}
	if _, err := store.Open(failing.key); err == nil {
		t.Fatal("object remained after metadata failure")
	}
}
