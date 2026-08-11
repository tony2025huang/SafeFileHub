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

func TestWriteRollsBackStagingWhenOffsetPersistenceFails(t *testing.T) {
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
	base := &failingOffsetRepo{Repository: repo, fail: true}
	m := New(base, store, 3, time.Hour)
	s, err := m.Create(context.Background(), u.ID, r.ID, "/a.txt", 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Write(context.Background(), s.ID, 0, bytes.NewBufferString("hello")); err == nil {
		t.Fatal("Write succeeded")
	}
	persisted, _ := repo.UploadSessionByID(context.Background(), s.ID)
	f, err := store.OpenStaging(persisted.StagingPath)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(f)
	_ = f.Close()
	if len(body) != 0 {
		t.Fatalf("staging length = %d, want 0 after persistence failure", len(body))
	}
	base.fail = false
	if got, err := m.Write(context.Background(), s.ID, 0, bytes.NewBufferString("hello")); err != nil || got != 5 {
		t.Fatalf("retry = %d, %v", got, err)
	}
}

type failingOffsetRepo struct {
	*db.Repository
	fail bool
}

func (r *failingOffsetRepo) UpdateUploadOffset(ctx context.Context, id string, expected, offset int64) error {
	if r.fail {
		return io.ErrUnexpectedEOF
	}
	return r.Repository.UpdateUploadOffset(ctx, id, expected, offset)
}

func TestCancelKeepsSessionWhenStagingRemovalFails(t *testing.T) {
	root := t.TempDir()
	store, err := storage.NewObjectStore(root)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := db.Open(context.Background(), root+"/meta.db")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	u, _ := repo.CreateUser(context.Background(), db.User{Username: "u", PasswordHash: "x"})
	r, _ := repo.CreateStorageRoot(context.Background(), db.StorageRoot{Name: "r", Path: root})
	m := New(repo, store, 1, time.Hour)
	s, err := m.Create(context.Background(), u.ID, r.ID, "/a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Cancel(context.Background(), s.ID); err == nil {
		t.Fatal("Cancel succeeded despite failed staging removal")
	}
	if _, err := repo.UploadSessionByID(context.Background(), s.ID); err != nil {
		t.Fatalf("session was deleted after failed staging cleanup: %v", err)
	}
}

func TestWriteRollsBackPartialReaderFailure(t *testing.T) {
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
	m := New(repo, store, 1, time.Hour)
	s, err := m.Create(context.Background(), u.ID, r.ID, "/a", 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Write(context.Background(), s.ID, 0, &partialFailureReader{}); err == nil {
		t.Fatal("Write succeeded")
	}
	got, _ := repo.UploadSessionByID(context.Background(), s.ID)
	f, err := store.OpenStaging(got.StagingPath)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(f)
	_ = f.Close()
	if len(body) != 0 || got.Offset != 0 {
		t.Fatalf("after failed write: offset=%d len=%d", got.Offset, len(body))
	}
}

type partialFailureReader struct{ sent bool }

func (r *partialFailureReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, io.ErrUnexpectedEOF
	}
	copy(p, "abc")
	r.sent = true
	return 3, nil
}

func TestCancelKeepsMetadataWhenDatabaseDeleteFails(t *testing.T) {
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
	wrapped := &failingDeleteRepo{Repository: repo, fail: true}
	m := New(wrapped, store, 1, time.Hour)
	s, err := m.Create(context.Background(), u.ID, r.ID, "/a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Cancel(context.Background(), s.ID); err == nil {
		t.Fatal("Cancel succeeded")
	}
	if _, err := repo.UploadSessionByID(context.Background(), s.ID); err != nil {
		t.Fatalf("metadata not retained: %v", err)
	}
	wrapped.fail = false
	if err := m.Cancel(context.Background(), s.ID); err != nil {
		t.Fatalf("retry cancel: %v", err)
	}
}

type failingDeleteRepo struct {
	*db.Repository
	fail bool
}

func (r *failingDeleteRepo) DeleteUploadSession(ctx context.Context, id string) error {
	if r.fail {
		return io.ErrUnexpectedEOF
	}
	return r.Repository.DeleteUploadSession(ctx, id)
}
