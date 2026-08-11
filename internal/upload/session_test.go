package upload

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
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

func TestCancelWaitsForWriteAcrossIndependentRepositories(t *testing.T) {
	root := t.TempDir()
	store1, err := storage.NewObjectStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store1.Close()
	store2, err := storage.NewObjectStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	repo1, err := db.Open(context.Background(), root+"/meta.db")
	if err != nil {
		t.Fatal(err)
	}
	defer repo1.Close()
	repo2, err := db.Open(context.Background(), root+"/meta.db")
	if err != nil {
		t.Fatal(err)
	}
	defer repo2.Close()
	u, _ := repo1.CreateUser(context.Background(), db.User{Username: "u", PasswordHash: "x"})
	r, _ := repo1.CreateStorageRoot(context.Background(), db.StorageRoot{Name: "r", Path: root})
	writer := New(repo1, store1, 1, time.Hour)
	canceller := New(repo2, store2, 1, time.Hour)
	s, err := writer.Create(context.Background(), u.ID, r.ID, "/a", 5)
	if err != nil {
		t.Fatal(err)
	}

	body := newBlockingReader("hello")
	writeDone := make(chan error, 1)
	go func() { _, err := writer.Write(context.Background(), s.ID, 0, body); writeDone <- err }()
	<-body.started
	cancelDone := make(chan error, 1)
	go func() { cancelDone <- canceller.Cancel(context.Background(), s.ID) }()
	select {
	case err := <-cancelDone:
		t.Fatalf("Cancel bypassed writer staging lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(body.release)
	if err := <-writeDone; err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := <-cancelDone; err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	assertSessionAndStagingConsistent(t, repo1, store1, s.ID)
}

func TestExpiryCleanupWaitsForWriteAcrossIndependentRepositories(t *testing.T) {
	root := t.TempDir()
	store1, err := storage.NewObjectStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store1.Close()
	store2, err := storage.NewObjectStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	repo1, err := db.Open(context.Background(), root+"/meta.db")
	if err != nil {
		t.Fatal(err)
	}
	defer repo1.Close()
	repo2, err := db.Open(context.Background(), root+"/meta.db")
	if err != nil {
		t.Fatal(err)
	}
	defer repo2.Close()
	u, _ := repo1.CreateUser(context.Background(), db.User{Username: "u", PasswordHash: "x"})
	r, _ := repo1.CreateStorageRoot(context.Background(), db.StorageRoot{Name: "r", Path: root})
	writer := New(repo1, store1, 1, 200*time.Millisecond)
	cleaner := New(repo2, store2, 1, time.Hour)
	s, err := writer.Create(context.Background(), u.ID, r.ID, "/a", 5)
	if err != nil {
		t.Fatal(err)
	}

	body := newBlockingReader("hello")
	writeDone := make(chan error, 1)
	go func() { _, err := writer.Write(context.Background(), s.ID, 0, body); writeDone <- err }()
	<-body.started
	time.Sleep(250 * time.Millisecond) // The writer already refreshed the live session under flock.
	expiryDone := make(chan error, 1)
	go func() { _, err := cleaner.Get(context.Background(), s.ID); expiryDone <- err }()
	select {
	case err := <-expiryDone:
		t.Fatalf("expiry cleanup bypassed writer staging lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(body.release)
	if err := <-writeDone; err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := <-expiryDone; !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("expired Get = %v, want ErrNotFound", err)
	}
	assertSessionAndStagingConsistent(t, repo1, store1, s.ID)
}

type blockingReader struct {
	data    []byte
	started chan struct{}
	release chan struct{}
	sent    bool
}

func newBlockingReader(data string) *blockingReader {
	return &blockingReader{data: []byte(data), started: make(chan struct{}), release: make(chan struct{})}
}

func (r *blockingReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		copy(p, r.data)
		close(r.started)
		return len(r.data), nil
	}
	<-r.release
	return 0, io.EOF
}

func assertSessionAndStagingConsistent(t *testing.T, repo *db.Repository, store *storage.ObjectStore, id string) {
	t.Helper()
	s, err := repo.UploadSessionByID(context.Background(), id)
	if errors.Is(err, db.ErrNotFound) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	f, err := store.OpenStaging(s.StagingPath)
	if err != nil {
		t.Fatalf("live session has no staging file: %v", err)
	}
	defer f.Close()
	body, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(body)) != s.Offset {
		t.Fatalf("session offset %d does not match staging length %d", s.Offset, len(body))
	}
}

func TestWriteDoesNotTruncateCommittedDataAfterCrossManagerStaleRead(t *testing.T) {
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
	stale := &staleReadRepo{Repository: repo, staleRead: make(chan struct{}), release: make(chan struct{})}
	first := New(repo, store, 1, time.Hour)
	second := New(stale, store, 1, time.Hour)
	s, err := first.Create(context.Background(), u.ID, r.ID, "/a", 10)
	if err != nil {
		t.Fatal(err)
	}

	secondDone := make(chan error, 1)
	go func() {
		_, err := second.Write(context.Background(), s.ID, 0, bytes.NewBufferString("world"))
		secondDone <- err
	}()
	<-stale.staleRead // second manager has materialized offset 0, but has not locked the file.
	if got, err := first.Write(context.Background(), s.ID, 0, bytes.NewBufferString("hello")); err != nil || got != 5 {
		t.Fatalf("first Write = %d, %v", got, err)
	}
	close(stale.release)
	if err := <-secondDone; !errors.Is(err, ErrOffset) {
		t.Fatalf("stale Write error = %v, want ErrOffset", err)
	}

	persisted, err := repo.UploadSessionByID(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	f, err := store.OpenStaging(persisted.StagingPath)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Offset != 5 || int64(len(body)) != persisted.Offset || string(body) != "hello" {
		t.Fatalf("committed staging corrupted: offset=%d length=%d body=%q", persisted.Offset, len(body), body)
	}
}

type staleReadRepo struct {
	*db.Repository
	staleRead chan struct{}
	release   chan struct{}
	once      sync.Once
}

func (r *staleReadRepo) UploadSessionByID(ctx context.Context, id string) (db.UploadSession, error) {
	s, err := r.Repository.UploadSessionByID(ctx, id)
	r.once.Do(func() {
		close(r.staleRead)
		<-r.release
	})
	return s, err
}

// TestUploadLifecycleHelper is a separate process used by the tests below. It
// deliberately opens its own SQLite pool and ObjectStore so the barrier proves
// the kernel flock protocol rather than a Manager-local mutex.
func TestUploadLifecycleHelper(t *testing.T) {
	if os.Getenv("SAFEFILEHUB_LIFECYCLE_HELPER") == "" {
		return
	}
	root, id, role := os.Getenv("SAFEFILEHUB_TEST_ROOT"), os.Getenv("SAFEFILEHUB_TEST_ID"), os.Getenv("SAFEFILEHUB_TEST_ROLE")
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
	m := New(repo, store, 1, time.Hour)
	marker := root + "/writer-ready"
	switch role {
	case "writer":
		_, err = m.Write(context.Background(), id, 0, &markerReader{ready: marker, release: root + "/release-writer", body: []byte("hello")})
	case "cancel":
		err = m.Cancel(context.Background(), id)
	case "expiry":
		_, err = m.Get(context.Background(), id)
	default:
		t.Fatalf("unknown helper role %q", role)
	}
	if err != nil && !(role == "expiry" && errors.Is(err, db.ErrNotFound)) {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+"/done-"+role, []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
}

type markerReader struct {
	ready, release string
	body           []byte
	sent           bool
}

func (r *markerReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		copy(p, r.body)
		if err := os.WriteFile(r.ready, []byte("locked"), 0600); err != nil {
			return 0, err
		}
		return len(r.body), nil
	}
	for {
		if _, err := os.Stat(r.release); err == nil {
			return 0, io.EOF
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLifecycleFlockAcrossProcessesCancelAndExpiry(t *testing.T) {
	for _, role := range []string{"cancel", "expiry"} {
		t.Run(role, func(t *testing.T) {
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
			u, _ := repo.CreateUser(context.Background(), db.User{Username: "u-" + role, PasswordHash: "x"})
			r, _ := repo.CreateStorageRoot(context.Background(), db.StorageRoot{Name: "r-" + role, Path: root})
			ttl := time.Hour
			if role == "expiry" {
				// The helper process must start and acquire flock while the session
				// is live; expire it only after the writer's marker confirms that.
				ttl = 3 * time.Second
			}
			s, err := New(repo, store, 1, ttl).Create(context.Background(), u.ID, r.ID, "/a", 5)
			if err != nil {
				t.Fatal(err)
			}
			writer := lifecycleHelper(root, s.ID, "writer")
			var writerOut bytes.Buffer
			writer.Stdout, writer.Stderr = &writerOut, &writerOut
			if err := writer.Start(); err != nil {
				t.Fatal(err)
			}
			waitForFile(t, root+"/writer-ready", 5*time.Second)
			if role == "expiry" {
				time.Sleep(3100 * time.Millisecond)
			}
			actor := lifecycleHelper(root, s.ID, role)
			var actorOut bytes.Buffer
			actor.Stdout, actor.Stderr = &actorOut, &actorOut
			if err := actor.Start(); err != nil {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
			if _, err := os.Stat(root + "/done-" + role); err == nil {
				t.Fatalf("%s bypassed writer flock", role)
			}
			// The writer has durable but intentionally uncommitted bytes while the
			// reader barrier is held; only assert that the active staging inode was
			// not unlinked by the separate process.
			live, err := repo.UploadSessionByID(context.Background(), s.ID)
			if err != nil {
				t.Fatal(err)
			}
			f, err := store.OpenStaging(live.StagingPath)
			if err != nil {
				t.Fatalf("actor unlinked active staging: %v", err)
			}
			_ = f.Close()
			if err := os.WriteFile(root+"/release-writer", []byte("go"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := writer.Wait(); err != nil {
				t.Fatalf("writer: %v\n%s", err, writerOut.String())
			}
			if err := actor.Wait(); err != nil {
				t.Fatalf("%s: %v\n%s", role, err, actorOut.String())
			}
			assertSessionAndStagingConsistent(t, repo, store, s.ID)
			// Retry is idempotent and converges after the first remover won.
			if err := New(repo, store, 1, time.Hour).Cancel(context.Background(), s.ID); !errors.Is(err, db.ErrNotFound) {
				t.Fatalf("retry cancel = %v, want ErrNotFound", err)
			}
		})
	}
}

func lifecycleHelper(root, id, role string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestUploadLifecycleHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "SAFEFILEHUB_LIFECYCLE_HELPER=1", "SAFEFILEHUB_TEST_ROOT="+root, "SAFEFILEHUB_TEST_ID="+id, "SAFEFILEHUB_TEST_ROLE="+role)
	return cmd
}
func waitForFile(t *testing.T, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(name); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", name)
}
