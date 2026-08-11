package db_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/example/safefilehub/internal/db"
)

func TestRepositoryCRUDAndMigrationIdempotence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "metadata.sqlite")

	repo, err := db.Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("run migrations a second time: %v", err)
	}
	assertWAL(t, databasePath)

	createdAt := time.Date(2026, 8, 11, 0, 0, 0, 123_000_000, time.FixedZone("non-UTC", -7*60*60))
	user, err := repo.CreateUser(ctx, db.User{Username: "alice", PasswordHash: "argon2id-hash", CreatedAt: createdAt})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if user.ID == 0 || !user.CreatedAt.Equal(createdAt.UTC()) || user.CreatedAt.Location() != time.UTC {
		t.Fatalf("unexpected user: %#v", user)
	}
	gotUser, err := repo.UserByUsername(ctx, "alice")
	if err != nil {
		t.Fatalf("read user: %v", err)
	}
	if gotUser.ID != user.ID || gotUser.PasswordHash != "argon2id-hash" {
		t.Fatalf("unexpected read user: %#v", gotUser)
	}
	if _, err := repo.CreateUser(ctx, db.User{Username: "alice", PasswordHash: "other"}); !errors.Is(err, db.ErrConflict) {
		t.Fatalf("duplicate username error = %v, want ErrConflict", err)
	}
	injectionName := "' OR 1=1 --"
	injectionUser, err := repo.CreateUser(ctx, db.User{Username: injectionName, PasswordHash: "safe"})
	if err != nil {
		t.Fatalf("create quoted username: %v", err)
	}
	gotInjectionUser, err := repo.UserByUsername(ctx, injectionName)
	if err != nil || gotInjectionUser.ID != injectionUser.ID {
		t.Fatalf("parameterized username lookup = %#v, %v", gotInjectionUser, err)
	}

	root, err := repo.CreateStorageRoot(ctx, db.StorageRoot{Name: "home", Path: "/srv/safefilehub"})
	if err != nil {
		t.Fatalf("create storage root: %v", err)
	}
	gotRoot, err := repo.StorageRootByID(ctx, root.ID)
	if err != nil || gotRoot.Path != root.Path {
		t.Fatalf("read storage root = %#v, %v", gotRoot, err)
	}

	file, err := repo.CreateFile(ctx, db.File{RootID: root.ID, LogicalPath: "/reports/q3.txt", ObjectKey: "objects/7f/q3", Size: 42, CreatedByUserID: user.ID})
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	gotFile, err := repo.FileByRootAndPath(ctx, root.ID, "/reports/q3.txt")
	if err != nil || gotFile.ID != file.ID || gotFile.Size != 42 {
		t.Fatalf("read file = %#v, %v", gotFile, err)
	}
	if _, err := repo.FileByRootAndPath(ctx, root.ID, "' OR 1=1 --"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("parameterized file lookup error = %v, want ErrNotFound", err)
	}

	permission, err := repo.CreatePermission(ctx, db.Permission{UserID: user.ID, RootID: root.ID, PathPrefix: "/reports", Action: " READ ", Allow: true})
	if err != nil {
		t.Fatalf("create permission: %v", err)
	}
	if permission.Action != "read" {
		t.Fatalf("permission action = %q, want normalized read", permission.Action)
	}
	permissions, err := repo.PermissionsForUserAndRoot(ctx, user.ID, root.ID)
	if err != nil || len(permissions) != 1 || permissions[0].ID != permission.ID {
		t.Fatalf("read permissions = %#v, %v", permissions, err)
	}

	session, err := repo.CreateUploadSession(ctx, db.UploadSession{ID: "upload-1", UserID: user.ID, RootID: root.ID, LogicalPath: "/reports/new.txt", StagingPath: "staging/upload-1.part", Offset: 7, ExpiresAt: createdAt.Add(time.Hour)})
	if err != nil {
		t.Fatalf("create upload session: %v", err)
	}
	gotSession, err := repo.UploadSessionByID(ctx, session.ID)
	if err != nil || gotSession.Offset != 7 || !gotSession.ExpiresAt.Equal(session.ExpiresAt) {
		t.Fatalf("read upload session = %#v, %v", gotSession, err)
	}

	event, err := repo.CreateAuditEvent(ctx, db.AuditEvent{UserID: user.ID, RootID: root.ID, Action: "file.create", LogicalPath: "/reports/q3.txt", Detail: "uploaded"})
	if err != nil {
		t.Fatalf("create audit event: %v", err)
	}
	events, err := repo.AuditEventsForUser(ctx, user.ID)
	if err != nil || len(events) != 1 || events[0].ID != event.ID || events[0].CreatedAt.Location() != time.UTC {
		t.Fatalf("read audit events = %#v, %v", events, err)
	}
}

func assertWAL(t *testing.T, databasePath string) {
	t.Helper()
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open database for WAL check: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	var journalMode string
	if err := database.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("read SQLite journal mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("SQLite journal mode = %q, want wal", journalMode)
	}
}

func TestCompleteUploadRejectsExpiredActiveSessionWithoutCreatingFile(t *testing.T) {
	ctx := context.Background()
	repo, err := db.Open(ctx, filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	u, err := repo.CreateUser(ctx, db.User{Username: "complete-expired", PasswordHash: "x"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.CreateStorageRoot(ctx, db.StorageRoot{Name: "complete-expired", Path: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateUploadSession(ctx, db.UploadSession{ID: "expired-complete", UserID: u.ID, RootID: r.ID, LogicalPath: "/expired", StagingPath: "staging/expired-complete.part", Length: 1, Offset: 1, ExpiresAt: time.Now().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	err = repo.CompleteUpload(ctx, db.File{RootID: r.ID, LogicalPath: "/expired", ObjectKey: "objects/aa/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Size: 1, CreatedByUserID: u.ID}, "expired-complete")
	if !errors.Is(err, db.ErrConflict) {
		t.Fatalf("CompleteUpload error = %v, want ErrConflict", err)
	}
	if _, err := repo.FileByRootAndPath(ctx, r.ID, "/expired"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("expired completion made file visible: %v", err)
	}
}
