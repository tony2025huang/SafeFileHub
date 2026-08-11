package publishedrecovery

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/storage"
)

// Recovery must work after a process restart: its only inputs are durable rows
// and opaque objects, not in-memory state from the failed request.
func TestRecoverIsIdempotentAfterConcurrentPublicationFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo, err := db.Open(ctx, filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	user, err := repo.CreateUser(ctx, db.User{Username: "recovery-idempotent", PasswordHash: "x"})
	if err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateStorageRoot(ctx, db.StorageRoot{Name: "root", Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewObjectStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cleanupKey, err := store.CreateEmpty("/cleanup")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueObjectCleanup(ctx, cleanupKey, "concurrent metadata failure"); err != nil {
		t.Fatal(err)
	}
	tombstoneKey, err := store.CreateEmpty("/tombstone")
	if err != nil {
		t.Fatal(err)
	}
	file, err := repo.CreateFile(ctx, db.File{RootID: root.ID, LogicalPath: "/tombstone", ObjectKey: tombstoneKey, CreatedByUserID: user.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.BeginFileDelete(ctx, file.ID, tombstoneKey); err != nil {
		t.Fatal(err)
	}
	first, err := Recover(ctx, repo, store, 16)
	if err != nil {
		t.Fatal(err)
	}
	if first.CleanupCompleted != 1 || first.TombstonesFinalized != 1 {
		t.Fatalf("first recovery=%+v", first)
	}
	second, err := Recover(ctx, repo, store, 16)
	if err != nil {
		t.Fatal(err)
	}
	if second.CleanupChecked != 0 || second.TombstonesChecked != 0 {
		t.Fatalf("second recovery was not idempotent: %+v", second)
	}
	if _, err := repo.FileForDeletion(ctx, file.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("tombstone remains after repeated recovery: %v", err)
	}
	jobs, err := repo.ObjectCleanupJobs(ctx, 16)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("cleanup jobs=%v err=%v", jobs, err)
	}
}

func TestRecoverReconcilesDurableCleanupAndTombstonesAfterRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo, err := db.Open(ctx, filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	u, err := repo.CreateUser(ctx, db.User{Username: "recovery", PasswordHash: "x"})
	if err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateStorageRoot(ctx, db.StorageRoot{Name: "root", Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewObjectStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cleanupKey, err := store.CreateEmpty("/cleanup")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueObjectCleanup(ctx, cleanupKey, "test"); err != nil {
		t.Fatal(err)
	}
	tombstoneKey, err := store.CreateEmpty("/tombstone")
	if err != nil {
		t.Fatal(err)
	}
	file, err := repo.CreateFile(ctx, db.File{RootID: root.ID, LogicalPath: "/tombstone", ObjectKey: tombstoneKey, CreatedByUserID: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.BeginFileDelete(ctx, file.ID, tombstoneKey); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// A newly opened store represents a restarted service.
	restarted, err := storage.NewObjectStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	report, err := Recover(ctx, repo, restarted, 64)
	if err != nil {
		t.Fatal(err)
	}
	if report.CleanupChecked != 1 || report.CleanupCompleted != 1 || report.TombstonesChecked != 1 || report.TombstonesFinalized != 1 {
		t.Fatalf("report=%+v", report)
	}
	if _, err := restarted.Open(cleanupKey); err == nil {
		t.Fatal("cleanup object remains")
	}
	if _, err := restarted.Open(tombstoneKey); err == nil {
		t.Fatal("tombstone object remains")
	}
	if _, err := repo.FileForDeletion(ctx, file.ID); err == nil {
		t.Fatal("tombstone metadata remains")
	}
	jobs, err := repo.ObjectCleanupJobs(ctx, 64)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("jobs=%v err=%v", jobs, err)
	}
}
