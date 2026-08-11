package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

func TestForeignKeysAreEnabledForEveryPooledConnection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, err := Open(ctx, filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	repo.db.SetMaxOpenConns(3)
	repo.db.SetMaxIdleConns(3)
	statements := []string{
		"INSERT INTO files (root_id, logical_path, object_key, size, created_by_user_id, created_at, updated_at) VALUES (999, '/orphan-file', 'orphan-file', 0, 999, 0, 0)",
		"INSERT INTO permissions (user_id, root_id, path_prefix, action, allow, created_at, updated_at) VALUES (999, 999, '/', 'read', 1, 0, 0)",
		"INSERT INTO upload_sessions (id, user_id, root_id, logical_path, staging_path, offset, expires_at, created_at, updated_at) VALUES ('orphan-upload', 999, 999, '/orphan-upload', 'orphan-upload', 0, 0, 0, 0)",
	}

	connections := make([]*sql.Conn, 0, len(statements))
	for range statements {
		conn, err := repo.db.Conn(ctx)
		if err != nil {
			t.Fatalf("acquire pooled connection: %v", err)
		}
		connections = append(connections, conn)
	}
	t.Cleanup(func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	})

	var wg sync.WaitGroup
	errs := make(chan error, len(statements))
	for i, statement := range statements {
		wg.Add(1)
		go func(conn *sql.Conn, statement string) {
			defer wg.Done()
			_, err := conn.ExecContext(ctx, statement)
			errs <- err
		}(connections[i], statement)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err == nil {
			t.Fatal("orphan insert succeeded; foreign keys must be enabled on every pooled connection")
		}
	}
}

func TestMigrateRecordsVersionsAndDoesNotReplayAppliedMigrations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, err := Open(ctx, filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	var versions []string
	rows, err := repo.db.QueryContext(ctx, "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		t.Fatalf("read schema migrations: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			t.Fatalf("scan migration version: %v", err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate migration versions: %v", err)
	}
	if want := []string{"001_initial", "002_permission_denies"}; fmt.Sprint(versions) != fmt.Sprint(want) {
		t.Fatalf("migration versions = %v, want %v", versions, want)
	}

	if _, err := repo.db.ExecContext(ctx, "DROP TABLE audit_events"); err != nil {
		t.Fatalf("drop table to detect migration replay: %v", err)
	}
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("migrate an already-versioned database: %v", err)
	}
	_, err = repo.db.ExecContext(ctx, "SELECT 1 FROM audit_events")
	if err == nil {
		t.Fatal("applied migration was replayed")
	}
}

func TestMigrationVersionsAreAppliedInOrder(t *testing.T) {
	files, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	versions := make([]string, 0, len(files))
	for _, file := range files {
		versions = append(versions, file.Name())
	}
	if !sort.StringsAreSorted(versions) {
		t.Fatalf("embedded migration filenames are not sorted: %v", versions)
	}
}
