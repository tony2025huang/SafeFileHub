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

type expireBeforePublishRepo struct {
	*db.Repository
	id      string
	reads   int
	expired bool
}

func (r *expireBeforePublishRepo) UploadSessionByID(ctx context.Context, id string) (db.UploadSession, error) {
	s, err := r.Repository.UploadSessionByID(ctx, id)
	if err == nil && id == r.id {
		r.reads++
	}
	if err == nil && id == r.id && r.reads == 3 && !r.expired {
		r.expired = true
		if err := r.UpdateUploadStatus(ctx, id, "active", "cancelled"); err != nil {
			return db.UploadSession{}, err
		}
		return r.Repository.UploadSessionByID(ctx, id)
	}
	return s, err
}

func TestCompleteRechecksLifecycleBeforePublishAndRestoresStaging(t *testing.T) {
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
	u, _ := repo.CreateUser(context.Background(), db.User{Username: "recheck", PasswordHash: "x"})
	r, _ := repo.CreateStorageRoot(context.Background(), db.StorageRoot{Name: "recheck", Path: root})
	base := New(repo, store, 8, time.Hour)
	s, err := base.Create(context.Background(), u.ID, r.ID, "/recheck", 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.Write(context.Background(), s.ID, 0, bytes.NewBufferString("hello")); err != nil {
		t.Fatal(err)
	}
	wrapped := &expireBeforePublishRepo{Repository: repo, id: s.ID}
	m := New(wrapped, store, 8, time.Hour)
	if err := m.Complete(context.Background(), s.ID, ""); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("Complete error = %v, want ErrNotFound", err)
	}
	if _, err := repo.FileByRootAndPath(context.Background(), r.ID, "/recheck"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("expired upload became visible: %v", err)
	}
	persisted, err := repo.UploadSessionByID(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	f, err := store.OpenStaging(persisted.StagingPath)
	if err != nil {
		t.Fatalf("staging not retained: %v", err)
	}
	_ = f.Close()
}
