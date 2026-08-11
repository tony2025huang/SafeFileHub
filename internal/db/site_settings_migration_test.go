package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestSiteSettingsMigrationCreatesDefaultSingletonAndMD5Schema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, err := Open(ctx, filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	var (
		id, filingEnabled, md5Enabled                    int
		siteName, primaryColor, filingText               string
		loginLogoAssetID, navLogoAssetID, faviconAssetID sql.NullInt64
	)
	if err := repo.db.QueryRowContext(ctx, `
		SELECT id, site_name, primary_color, filing_enabled, filing_text,
		       md5_enabled, login_logo_asset_id, nav_logo_asset_id, favicon_asset_id
		FROM site_settings WHERE id = 1`).Scan(
		&id, &siteName, &primaryColor, &filingEnabled, &filingText,
		&md5Enabled, &loginLogoAssetID, &navLogoAssetID, &faviconAssetID,
	); err != nil {
		t.Fatalf("read default site settings: %v", err)
	}
	if id != 1 || siteName != "SafeFileHub" || primaryColor != "#2563EB" || filingEnabled != 0 || filingText != "" || md5Enabled != 0 || loginLogoAssetID.Valid || navLogoAssetID.Valid || faviconAssetID.Valid {
		t.Fatalf("default site settings = (%d, %q, %q, %d, %q, %d, %v, %v, %v), want compatibility defaults", id, siteName, primaryColor, filingEnabled, filingText, md5Enabled, loginLogoAssetID, navLogoAssetID, faviconAssetID)
	}

	assertSchemaRejects(t, repo.db, `INSERT INTO site_settings (id, site_name, primary_color, filing_enabled, filing_text, md5_enabled, created_at, updated_at) VALUES (2, 'Other', '#000000', 0, '', 0, 0, 0)`, "second settings singleton")
	assertSchemaRejects(t, repo.db, `UPDATE site_settings SET md5_enabled = 2 WHERE id = 1`, "invalid MD5 switch")
	assertSchemaRejects(t, repo.db, `INSERT INTO site_assets (kind, opaque_key, content_type, size, width, height, created_at, updated_at) VALUES ('favicon', 'asset-1', 'image/png', -1, 1, 1, 0, 0)`, "negative asset size")
	assertSchemaRejects(t, repo.db, `INSERT INTO md5_tasks (file_id, status, attempts, max_attempts, available_at, created_at, updated_at) VALUES (999, 'pending', 4, 3, 0, 0, 0)`, "task beyond retry bound")
}

func TestSiteSettingsMigrationUpgradesExistingDatabaseWithoutBackfill(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy.sqlite")
	database, err := sql.Open("sqlite", sqliteDSN(databasePath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, mustReadMigration(t, "001_initial.sql")); err != nil {
		_ = database.Close()
		t.Fatalf("apply legacy migration: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL);
		INSERT INTO schema_migrations VALUES
			('001_initial', 0), ('002_permission_denies', 0), ('003_upload_session_length', 0),
			('004_upload_session_lifecycle', 0), ('005_audit_event_status', 0), ('006_directories', 0),
			('007_published_lifecycle', 0), ('008_published_parent_invariant', 0);
		INSERT INTO users (id, username, password_hash, disabled, created_at, updated_at) VALUES (1, 'user', 'hash', 0, 0, 0);
		INSERT INTO storage_roots (id, name, path, created_at, updated_at) VALUES (1, 'root', '/root', 0, 0);
		INSERT INTO files (id, root_id, logical_path, object_key, size, created_by_user_id, created_at, updated_at) VALUES (1, 1, '/legacy.txt', 'legacy-key', 1, 1, 0, 0);`); err != nil {
		_ = database.Close()
		t.Fatalf("seed legacy database: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	repo, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("upgrade legacy database: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	var status string
	var digest string
	if err := repo.db.QueryRowContext(ctx, `SELECT md5_status, md5_digest FROM files WHERE id = 1`).Scan(&status, &digest); err != nil {
		t.Fatalf("read upgraded legacy file: %v", err)
	}
	if status != "disabled" || digest != "" {
		t.Fatalf("legacy file MD5 = (%q, %q), want disabled with no digest", status, digest)
	}
	var taskCount int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM md5_tasks WHERE file_id = 1`).Scan(&taskCount); err != nil {
		t.Fatalf("count legacy MD5 tasks: %v", err)
	}
	if taskCount != 0 {
		t.Fatalf("legacy file has %d MD5 tasks, want no backfill", taskCount)
	}
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	var settingsCount int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM site_settings`).Scan(&settingsCount); err != nil {
		t.Fatalf("count site settings after repeat migration: %v", err)
	}
	if settingsCount != 1 {
		t.Fatalf("site settings rows after repeat migration = %d, want 1", settingsCount)
	}
}

func assertSchemaRejects(t *testing.T, database *sql.DB, statement, name string) {
	t.Helper()
	if _, err := database.Exec(statement); err == nil {
		t.Fatalf("%s succeeded", name)
	}
}
