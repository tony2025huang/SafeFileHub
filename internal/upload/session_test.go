package upload

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/storage"
)

func TestSessionPersistsAndWritesAtOffset(t *testing.T) {
	root := t.TempDir()
	store, err := storage.NewObjectStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repo, err := db.Open(context.Background(), root+"/meta.db")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	u, _ := repo.CreateUser(context.Background(), db.User{Username: "u", PasswordHash: "x"})
	r, _ := repo.CreateStorageRoot(context.Background(), db.StorageRoot{Name: "r", Path: root})
	m := New(repo, store, 3, time.Hour)
	s, err := m.Create(context.Background(), u.ID, r.ID, "/a.txt", 5)
	if err != nil {
		t.Fatal(err)
	}
	if s.Offset != 0 || s.Length != 5 {
		t.Fatalf("session %#v", s)
	}
	if got, err := m.Write(context.Background(), s.ID, 0, bytes.NewBufferString("hello")); err != nil || got != 5 {
		t.Fatalf("Write = %d, %v", got, err)
	}
	got, err := repo.UploadSessionByID(context.Background(), s.ID)
	if err != nil || got.Offset != 5 || got.Length != 5 {
		t.Fatalf("persisted = %#v, %v", got, err)
	}
	f, err := store.OpenStaging(got.StagingPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	body, _ := io.ReadAll(f)
	if string(body) != "hello" {
		t.Fatalf("body %q", body)
	}
}
