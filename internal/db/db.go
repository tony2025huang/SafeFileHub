// Package db manages SafeFileHub's SQLite metadata database.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Repository provides parameterized access to SafeFileHub metadata.
type Repository struct {
	db *sql.DB
}

// Open opens a SQLite database, enables foreign-key enforcement on every
// connection, configures WAL when supported, and applies embedded migrations.
func Open(ctx context.Context, databasePath string) (*Repository, error) {
	database, err := sql.Open("sqlite", sqliteDSN(databasePath))
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	repo := &Repository{db: database}
	if err := repo.configure(ctx); err != nil {
		_ = database.Close()
		return nil, err
	}
	if err := repo.Migrate(ctx); err != nil {
		_ = database.Close()
		return nil, err
	}
	return repo, nil
}

// sqliteDSN uses modernc.org/sqlite's per-connection pragma support. PRAGMA
// foreign_keys is connection-local in SQLite, so executing it once on sql.DB
// is insufficient when the pool opens further connections.
func sqliteDSN(databasePath string) string {
	separator := "?"
	if strings.Contains(databasePath, "?") {
		separator = "&"
	}
	return databasePath + separator + "_pragma=foreign_keys(ON)"
}

func (r *Repository) configure(ctx context.Context) error {
	// SQLite may fall back for databases where WAL is unavailable; either result
	// is safe, and the repository does not rely on a particular journal mode.
	if _, err := r.db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
		return fmt.Errorf("enable SQLite WAL: %w", err)
	}
	return nil
}

type migration struct {
	version string
	sql     string
}

// Migrate applies each unapplied embedded migration in filename order. Each
// migration and its version record commit atomically in one transaction.
func (r *Repository) Migrate(ctx context.Context) error {
	migrations, err := embeddedMigrations()
	if err != nil {
		return err
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migrations transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema migrations table: %w", err)
	}

	for _, migration := range migrations {
		var applied bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = ?)", migration.version).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %s: %w", migration.version, err)
		}
		if applied {
			continue
		}
		if _, err := tx.ExecContext(ctx, migration.sql); err != nil {
			return fmt.Errorf("apply migration %s: %w", migration.version, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)", migration.version, time.Now().UTC().UnixNano()); err != nil {
			return fmt.Errorf("record migration %s: %w", migration.version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migrations transaction: %w", err)
	}
	return nil
}

func embeddedMigrations() ([]migration, error) {
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || path.Ext(entry.Name()) != ".sql" {
			continue
		}
		filename := entry.Name()
		sql, err := migrationFiles.ReadFile("migrations/" + filename)
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", filename, err)
		}
		migrations = append(migrations, migration{version: strings.TrimSuffix(filename, ".sql"), sql: string(sql)})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	return migrations, nil
}

// Close releases the underlying database handle.
func (r *Repository) Close() error {
	return r.db.Close()
}
