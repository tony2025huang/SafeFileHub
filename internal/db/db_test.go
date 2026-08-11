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
	if want := []string{"001_initial", "002_permission_denies", "003_upload_session_length", "004_upload_session_lifecycle", "005_audit_event_status", "006_directories", "007_published_lifecycle", "008_published_parent_invariant", "009_site_settings_md5", "010_site_asset_cleanup"}; fmt.Sprint(versions) != fmt.Sprint(want) {
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

func TestPermissionMigrationUpgradesExisting001Data(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy.sqlite")
	database, err := sql.Open("sqlite", sqliteDSN(databasePath))
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if _, err := database.ExecContext(ctx, mustReadMigration(t, "001_initial.sql")); err != nil {
		_ = database.Close()
		t.Fatalf("apply 001: %v", err)
	}
	if _, err := database.ExecContext(ctx, `CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL); INSERT INTO schema_migrations VALUES ('001_initial', 0); INSERT INTO users (id, username, password_hash, disabled, created_at, updated_at) VALUES (7, 'user', 'hash', 0, 0, 0); INSERT INTO storage_roots (id, name, path, created_at, updated_at) VALUES (9, 'root', '/root', 0, 0); INSERT INTO permissions (id, user_id, root_id, path_prefix, action, allow, created_at, updated_at) VALUES (11, 7, 9, '/docs', 'read', 1, 0, 0);`); err != nil {
		_ = database.Close()
		t.Fatalf("seed 001 database: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	repo, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("upgrade legacy database: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	var id, userID, rootID int64
	if err := repo.db.QueryRowContext(ctx, "SELECT id, user_id, root_id FROM permissions").Scan(&id, &userID, &rootID); err != nil || id != 11 || userID != 7 || rootID != 9 {
		t.Fatalf("upgraded permission identity = (%d, %d, %d), %v", id, userID, rootID, err)
	}
	if _, err := repo.db.ExecContext(ctx, "INSERT INTO permissions (user_id, root_id, path_prefix, action, allow, created_at, updated_at) VALUES (7, 9, '/docs', 'read', 0, 0, 0)"); err != nil {
		t.Fatalf("insert equal-prefix deny: %v", err)
	}
	if _, err := repo.db.ExecContext(ctx, "INSERT INTO permissions (user_id, root_id, path_prefix, action, allow, created_at, updated_at) VALUES (7, 9, '/docs', 'read', 1, 0, 0)"); err == nil {
		t.Fatal("duplicate permission with same allow value succeeded")
	}
	var indexCount int
	if err := repo.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_index_list('permissions') WHERE name = 'permissions_user_root_idx'").Scan(&indexCount); err != nil || indexCount != 1 {
		t.Fatalf("permissions_user_root_idx = %d, %v", indexCount, err)
	}
	if _, err := repo.db.ExecContext(ctx, "DELETE FROM users WHERE id = 7"); err != nil {
		t.Fatalf("delete parent user: %v", err)
	}
	var count int
	if err := repo.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM permissions").Scan(&count); err != nil || count != 0 {
		t.Fatalf("cascaded permissions count = %d, %v", count, err)
	}
}

func mustReadMigration(t *testing.T, name string) string {
	t.Helper()
	contents, err := migrationFiles.ReadFile("migrations/" + name)
	if err != nil {
		t.Fatalf("read migration %s: %v", name, err)
	}
	return string(contents)
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

func TestUploadLengthMigrationUpgradesExistingSessions(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy.sqlite")
	database, err := sql.Open("sqlite", sqliteDSN(databasePath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.ExecContext(ctx, mustReadMigration(t, "001_initial.sql")); err != nil {
		t.Fatal(err)
	}
	if _, err = database.ExecContext(ctx, `CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL); INSERT INTO schema_migrations VALUES ('001_initial',0),('002_permission_denies',0); INSERT INTO users (id,username,password_hash,disabled,created_at,updated_at) VALUES (1,'u','x',0,0,0); INSERT INTO storage_roots (id,name,path,created_at,updated_at) VALUES (1,'r','/r',0,0); INSERT INTO upload_sessions (id,user_id,root_id,logical_path,staging_path,offset,expires_at,created_at,updated_at) VALUES ('legacy',1,1,'/a','staging/legacy.part',4,9,1,2);`); err != nil {
		t.Fatal(err)
	}
	if err = database.Close(); err != nil {
		t.Fatal(err)
	}
	repo, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	s, err := repo.UploadSessionByID(ctx, "legacy")
	if err != nil || s.Offset != 4 || s.Length != 0 {
		t.Fatalf("upgraded session = %#v, %v", s, err)
	}
	var indexCount int
	if err := repo.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_index_list('upload_sessions') WHERE name = 'upload_sessions_expires_idx'").Scan(&indexCount); err != nil || indexCount != 1 {
		t.Fatalf("expires index = %d, %v", indexCount, err)
	}
}
