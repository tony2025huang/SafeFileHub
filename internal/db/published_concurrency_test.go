package db

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

// Separate Repository values force SQLite through independent sql.DB pools,
// approximating separate server processes without spawning an unbounded load
// test. The configured busy timeout must turn writer contention into a durable
// conflict, never metadata corruption or a violated path uniqueness invariant.
func TestPublishedSQLiteMultiConnectionContentionIsConflictSafe(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "metadata.sqlite")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	owner, err := first.CreateUser(ctx, User{Username: "writer", PasswordHash: "x"})
	if err != nil {
		t.Fatal(err)
	}
	root, err := first.CreateStorageRoot(ctx, StorageRoot{Name: "root", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	const writers = 16
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		repo := first
		if i%2 != 0 {
			repo = second
		}
		wg.Add(1)
		go func(repo *Repository) {
			defer wg.Done()
			<-start
			_, err := repo.CreateDirectory(ctx, Directory{RootID: root.ID, LogicalPath: "/contended", CreatedByUserID: owner.ID})
			errs <- err
		}(repo)
	}
	close(start)
	wg.Wait()
	close(errs)
	created := 0
	for err := range errs {
		if err == nil {
			created++
			continue
		}
		if !isConflict(err) {
			t.Fatalf("contention error=%v, want conflict", err)
		}
	}
	if created != 1 {
		t.Fatalf("created=%d, want exactly one", created)
	}
	if _, err := second.DirectoryByRootAndPath(ctx, root.ID, "/contended"); err != nil {
		t.Fatalf("read contended row through second pool: %v", err)
	}
	var integrity string
	if err := first.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check=%q err=%v", integrity, err)
	}
}

func isConflict(err error) bool { return errors.Is(err, ErrConflict) }
