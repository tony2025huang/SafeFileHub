package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/storage"
)

func TestCompletePublishesOnlyFullMatchingUpload(t *testing.T) {
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
	m := New(repo, store, 8, time.Hour)
	s, err := m.Create(context.Background(), u.ID, r.ID, "/a", 5)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Complete(context.Background(), s.ID, ""); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("incomplete = %v", err)
	}
	if _, err := repo.FileByRootAndPath(context.Background(), r.ID, "/a"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("incomplete visible: %v", err)
	}
	if _, err := m.Write(context.Background(), s.ID, 0, bytes.NewBufferString("hello")); err != nil {
		t.Fatal(err)
	}
	if err := m.Complete(context.Background(), s.ID, "00"); !errors.Is(err, ErrChecksum) {
		t.Fatalf("wrong hash = %v", err)
	}
	sum := sha256.Sum256([]byte("hello"))
	if err := m.Complete(context.Background(), s.ID, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	f, err := repo.FileByRootAndPath(context.Background(), r.ID, "/a")
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.Open(f.ObjectKey)
	if err != nil {
		t.Fatal(err)
	}
	defer object.Close()
	b, _ := io.ReadAll(object)
	if string(b) != "hello" {
		t.Fatalf("object=%q", b)
	}
}
