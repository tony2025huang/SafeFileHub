// Package db manages SafeFileHub's SQLite metadata database.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Repository provides parameterized access to SafeFileHub metadata.
type Repository struct {
	db *sql.DB
}

// Open opens a SQLite database, enables foreign-key enforcement, configures WAL
// when supported, and applies the embedded migrations.
func Open(ctx context.Context, path string) (*Repository, error) {
	database, err := sql.Open("sqlite", path)
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

func (r *Repository) configure(ctx context.Context) error {
	if _, err := r.db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("enable SQLite foreign keys: %w", err)
	}
	// SQLite may fall back for databases where WAL is unavailable; either result
	// is safe, and the repository does not rely on a particular journal mode.
	if _, err := r.db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
		return fmt.Errorf("enable SQLite WAL: %w", err)
	}
	return nil
}

// Migrate applies every embedded migration. Migration SQL uses idempotent DDL,
// making it safe to call during each process start.
func (r *Repository) Migrate(ctx context.Context) error {
	migration, err := migrationFiles.ReadFile("migrations/001_initial.sql")
	if err != nil {
		return fmt.Errorf("read initial migration: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, string(migration)); err != nil {
		return fmt.Errorf("apply initial migration: %w", err)
	}
	return nil
}

// Close releases the underlying database handle.
func (r *Repository) Close() error {
	return r.db.Close()
}
