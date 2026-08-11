package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrConflict indicates a unique metadata value is already in use.
	ErrConflict = errors.New("metadata conflict")
	// ErrNotFound indicates requested metadata does not exist.
	ErrNotFound = errors.New("metadata not found")
)

type User struct {
	ID           int64
	Username     string
	PasswordHash string
	Disabled     bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type StorageRoot struct {
	ID        int64
	Name      string
	Path      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type File struct {
	ID              int64
	RootID          int64
	LogicalPath     string
	ObjectKey       string
	Size            int64
	CreatedByUserID int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type Permission struct {
	ID         int64
	UserID     int64
	RootID     int64
	PathPrefix string
	Action     string
	Allow      bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type UploadSession struct {
	ID          string
	UserID      int64
	RootID      int64
	LogicalPath string
	StagingPath string
	Offset      int64
	ExpiresAt   time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type AuditEvent struct {
	ID          int64
	UserID      int64
	RootID      int64
	Action      string
	LogicalPath string
	Detail      string
	CreatedAt   time.Time
}

func (r *Repository) CreateUser(ctx context.Context, user User) (User, error) {
	createdAt := utcOrNow(user.CreatedAt)
	updatedAt := utcOrNow(user.UpdatedAt)
	if user.UpdatedAt.IsZero() {
		updatedAt = createdAt
	}
	result, err := r.db.ExecContext(ctx, `INSERT INTO users (username, password_hash, disabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`, user.Username, user.PasswordHash, boolInt(user.Disabled), unixNano(createdAt), unixNano(updatedAt))
	if err != nil {
		return User{}, classifyError(err)
	}
	user.ID, err = result.LastInsertId()
	if err != nil {
		return User{}, fmt.Errorf("get created user id: %w", err)
	}
	user.CreatedAt, user.UpdatedAt = createdAt, updatedAt
	return user, nil
}

func (r *Repository) UserByUsername(ctx context.Context, username string) (User, error) {
	row := r.db.QueryRowContext(ctx, `SELECT id, username, password_hash, disabled, created_at, updated_at FROM users WHERE username = ?`, username)
	return scanUser(row)
}

func (r *Repository) CreateStorageRoot(ctx context.Context, root StorageRoot) (StorageRoot, error) {
	createdAt := utcOrNow(root.CreatedAt)
	updatedAt := utcOrNow(root.UpdatedAt)
	if root.UpdatedAt.IsZero() {
		updatedAt = createdAt
	}
	result, err := r.db.ExecContext(ctx, `INSERT INTO storage_roots (name, path, created_at, updated_at) VALUES (?, ?, ?, ?)`, root.Name, root.Path, unixNano(createdAt), unixNano(updatedAt))
	if err != nil {
		return StorageRoot{}, classifyError(err)
	}
	root.ID, err = result.LastInsertId()
	if err != nil {
		return StorageRoot{}, fmt.Errorf("get created storage root id: %w", err)
	}
	root.CreatedAt, root.UpdatedAt = createdAt, updatedAt
	return root, nil
}

func (r *Repository) StorageRootByID(ctx context.Context, id int64) (StorageRoot, error) {
	var root StorageRoot
	var createdAt, updatedAt int64
	err := r.db.QueryRowContext(ctx, `SELECT id, name, path, created_at, updated_at FROM storage_roots WHERE id = ?`, id).Scan(&root.ID, &root.Name, &root.Path, &createdAt, &updatedAt)
	if err != nil {
		return StorageRoot{}, classifyError(err)
	}
	root.CreatedAt, root.UpdatedAt = fromUnixNano(createdAt), fromUnixNano(updatedAt)
	return root, nil
}

func (r *Repository) CreateFile(ctx context.Context, file File) (File, error) {
	createdAt := utcOrNow(file.CreatedAt)
	updatedAt := utcOrNow(file.UpdatedAt)
	if file.UpdatedAt.IsZero() {
		updatedAt = createdAt
	}
	result, err := r.db.ExecContext(ctx, `INSERT INTO files (root_id, logical_path, object_key, size, created_by_user_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, file.RootID, file.LogicalPath, file.ObjectKey, file.Size, file.CreatedByUserID, unixNano(createdAt), unixNano(updatedAt))
	if err != nil {
		return File{}, classifyError(err)
	}
	file.ID, err = result.LastInsertId()
	if err != nil {
		return File{}, fmt.Errorf("get created file id: %w", err)
	}
	file.CreatedAt, file.UpdatedAt = createdAt, updatedAt
	return file, nil
}

func (r *Repository) FileByRootAndPath(ctx context.Context, rootID int64, logicalPath string) (File, error) {
	var file File
	var createdAt, updatedAt int64
	err := r.db.QueryRowContext(ctx, `SELECT id, root_id, logical_path, object_key, size, created_by_user_id, created_at, updated_at FROM files WHERE root_id = ? AND logical_path = ?`, rootID, logicalPath).Scan(&file.ID, &file.RootID, &file.LogicalPath, &file.ObjectKey, &file.Size, &file.CreatedByUserID, &createdAt, &updatedAt)
	if err != nil {
		return File{}, classifyError(err)
	}
	file.CreatedAt, file.UpdatedAt = fromUnixNano(createdAt), fromUnixNano(updatedAt)
	return file, nil
}

// FilesUnderRoot returns completed-file metadata candidates. Callers must
// apply authorization and logical-name policy before exposing any entry.
func (r *Repository) FilesUnderRoot(ctx context.Context, rootID int64) ([]File, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, root_id, logical_path, object_key, size, created_by_user_id, created_at, updated_at FROM files WHERE root_id = ? ORDER BY logical_path`, rootID)
	if err != nil {
		return nil, fmt.Errorf("query files under root: %w", err)
	}
	defer rows.Close()
	var files []File
	for rows.Next() {
		var file File
		var createdAt, updatedAt int64
		if err := rows.Scan(&file.ID, &file.RootID, &file.LogicalPath, &file.ObjectKey, &file.Size, &file.CreatedByUserID, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan file: %w", err)
		}
		file.CreatedAt, file.UpdatedAt = fromUnixNano(createdAt), fromUnixNano(updatedAt)
		files = append(files, file)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate files under root: %w", err)
	}
	return files, nil
}

func (r *Repository) CreatePermission(ctx context.Context, permission Permission) (Permission, error) {
	action := normalizeAction(permission.Action)
	if action == "" {
		return Permission{}, fmt.Errorf("invalid permission action %q", permission.Action)
	}
	createdAt := utcOrNow(permission.CreatedAt)
	updatedAt := utcOrNow(permission.UpdatedAt)
	if permission.UpdatedAt.IsZero() {
		updatedAt = createdAt
	}
	result, err := r.db.ExecContext(ctx, `INSERT INTO permissions (user_id, root_id, path_prefix, action, allow, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, permission.UserID, permission.RootID, permission.PathPrefix, action, boolInt(permission.Allow), unixNano(createdAt), unixNano(updatedAt))
	if err != nil {
		return Permission{}, classifyError(err)
	}
	permission.ID, err = result.LastInsertId()
	if err != nil {
		return Permission{}, fmt.Errorf("get created permission id: %w", err)
	}
	permission.Action, permission.CreatedAt, permission.UpdatedAt = action, createdAt, updatedAt
	return permission, nil
}

func (r *Repository) PermissionsForUserAndRoot(ctx context.Context, userID, rootID int64) ([]Permission, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, user_id, root_id, path_prefix, action, allow, created_at, updated_at FROM permissions WHERE user_id = ? AND root_id = ? ORDER BY id`, userID, rootID)
	if err != nil {
		return nil, fmt.Errorf("query permissions: %w", err)
	}
	defer rows.Close()
	var permissions []Permission
	for rows.Next() {
		var p Permission
		var allow int
		var createdAt, updatedAt int64
		if err := rows.Scan(&p.ID, &p.UserID, &p.RootID, &p.PathPrefix, &p.Action, &allow, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan permission: %w", err)
		}
		p.Allow, p.CreatedAt, p.UpdatedAt = allow != 0, fromUnixNano(createdAt), fromUnixNano(updatedAt)
		permissions = append(permissions, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate permissions: %w", err)
	}
	return permissions, nil
}

func (r *Repository) CreateUploadSession(ctx context.Context, session UploadSession) (UploadSession, error) {
	createdAt := utcOrNow(session.CreatedAt)
	updatedAt := utcOrNow(session.UpdatedAt)
	if session.UpdatedAt.IsZero() {
		updatedAt = createdAt
	}
	expiresAt := utcOrNow(session.ExpiresAt)
	result, err := r.db.ExecContext(ctx, `INSERT INTO upload_sessions (id, user_id, root_id, logical_path, staging_path, offset, expires_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, session.ID, session.UserID, session.RootID, session.LogicalPath, session.StagingPath, session.Offset, unixNano(expiresAt), unixNano(createdAt), unixNano(updatedAt))
	if err != nil {
		return UploadSession{}, classifyError(err)
	}
	if _, err := result.RowsAffected(); err != nil {
		return UploadSession{}, fmt.Errorf("confirm upload session creation: %w", err)
	}
	session.CreatedAt, session.UpdatedAt, session.ExpiresAt = createdAt, updatedAt, expiresAt
	return session, nil
}

func (r *Repository) UploadSessionByID(ctx context.Context, id string) (UploadSession, error) {
	var session UploadSession
	var expiresAt, createdAt, updatedAt int64
	err := r.db.QueryRowContext(ctx, `SELECT id, user_id, root_id, logical_path, staging_path, offset, expires_at, created_at, updated_at FROM upload_sessions WHERE id = ?`, id).Scan(&session.ID, &session.UserID, &session.RootID, &session.LogicalPath, &session.StagingPath, &session.Offset, &expiresAt, &createdAt, &updatedAt)
	if err != nil {
		return UploadSession{}, classifyError(err)
	}
	session.ExpiresAt, session.CreatedAt, session.UpdatedAt = fromUnixNano(expiresAt), fromUnixNano(createdAt), fromUnixNano(updatedAt)
	return session, nil
}

func (r *Repository) CreateAuditEvent(ctx context.Context, event AuditEvent) (AuditEvent, error) {
	createdAt := utcOrNow(event.CreatedAt)
	result, err := r.db.ExecContext(ctx, `INSERT INTO audit_events (user_id, root_id, action, logical_path, detail, created_at) VALUES (?, ?, ?, ?, ?, ?)`, event.UserID, event.RootID, event.Action, event.LogicalPath, event.Detail, unixNano(createdAt))
	if err != nil {
		return AuditEvent{}, classifyError(err)
	}
	event.ID, err = result.LastInsertId()
	if err != nil {
		return AuditEvent{}, fmt.Errorf("get created audit event id: %w", err)
	}
	event.CreatedAt = createdAt
	return event, nil
}

func (r *Repository) AuditEventsForUser(ctx context.Context, userID int64) ([]AuditEvent, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, user_id, root_id, action, logical_path, detail, created_at FROM audit_events WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("query audit events: %w", err)
	}
	defer rows.Close()
	var events []AuditEvent
	for rows.Next() {
		var event AuditEvent
		var createdAt int64
		if err := rows.Scan(&event.ID, &event.UserID, &event.RootID, &event.Action, &event.LogicalPath, &event.Detail, &createdAt); err != nil {
			return nil, fmt.Errorf("scan audit event: %w", err)
		}
		event.CreatedAt = fromUnixNano(createdAt)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audit events: %w", err)
	}
	return events, nil
}

func scanUser(row *sql.Row) (User, error) {
	var user User
	var disabled int
	var createdAt, updatedAt int64
	if err := row.Scan(&user.ID, &user.Username, &user.PasswordHash, &disabled, &createdAt, &updatedAt); err != nil {
		return User{}, classifyError(err)
	}
	user.Disabled, user.CreatedAt, user.UpdatedAt = disabled != 0, fromUnixNano(createdAt), fromUnixNano(updatedAt)
	return user, nil
}

func normalizeAction(action string) string {
	action = strings.ToLower(strings.TrimSpace(action))
	switch action {
	case "read", "write", "delete", "archive":
		return action
	default:
		return ""
	}
}

func utcOrNow(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}

func unixNano(value time.Time) int64     { return value.UTC().UnixNano() }
func fromUnixNano(value int64) time.Time { return time.Unix(0, value).UTC() }
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func classifyError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "unique constraint") || strings.Contains(message, "constraint failed") {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return err
}
