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
	MD5Status       string
	MD5Digest       string
	MD5Error        string
}

const (
	MD5Disabled  = "disabled"
	MD5Pending   = "pending"
	MD5Computing = "computing"
	MD5Ready     = "ready"
	MD5Failed    = "failed"
)

type SiteSettings struct {
	SiteName, PrimaryColor, FilingText               string
	FilingEnabled, MD5Enabled                        bool
	LoginLogoAssetID, NavLogoAssetID, FaviconAssetID int64
	CreatedAt, UpdatedAt                             time.Time
}

type SiteAsset struct {
	ID                           int64
	Kind, OpaqueKey, ContentType string
	Size, Width, Height          int64
	CreatedAt, UpdatedAt         time.Time
}

type MD5Task struct {
	FileID                 int64
	Status                 string
	Attempts, MaxAttempts  int
	AvailableAt, ClaimedAt time.Time
}

type Directory struct {
	ID              int64
	RootID          int64
	LogicalPath     string
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
	Length      int64
	Status      string
	ExpiresAt   time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type ObjectCleanupJob struct{ ObjectKey, Reason string }

type AuditEvent struct {
	ID          int64
	UserID      int64
	RootID      int64
	Action      string
	LogicalPath string
	Detail      string
	Status      int
	CreatedAt   time.Time
}

func (r *Repository) SiteSettings(ctx context.Context) (SiteSettings, error) {
	var s SiteSettings
	var filing, md5 int
	var login, nav, favicon sql.NullInt64
	var created, updated int64
	err := r.db.QueryRowContext(ctx, `SELECT site_name, primary_color, filing_enabled, filing_text, md5_enabled, login_logo_asset_id, nav_logo_asset_id, favicon_asset_id, created_at, updated_at FROM site_settings WHERE id=1`).Scan(&s.SiteName, &s.PrimaryColor, &filing, &s.FilingText, &md5, &login, &nav, &favicon, &created, &updated)
	if err != nil {
		return SiteSettings{}, classifyError(err)
	}
	s.FilingEnabled, s.MD5Enabled = filing != 0, md5 != 0
	s.LoginLogoAssetID, s.NavLogoAssetID, s.FaviconAssetID = login.Int64, nav.Int64, favicon.Int64
	s.CreatedAt, s.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
	return s, nil
}

func validSiteSettings(s SiteSettings) bool {
	if strings.TrimSpace(s.SiteName) == "" || len(s.SiteName) > 200 || len(s.FilingText) > 500 {
		return false
	}
	if len(s.PrimaryColor) != 7 || s.PrimaryColor[0] != '#' {
		return false
	}
	for _, c := range s.PrimaryColor[1:] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}
func (r *Repository) UpdateSiteSettings(ctx context.Context, s SiteSettings) error {
	if !validSiteSettings(s) {
		return errors.New("invalid site settings")
	}
	result, err := r.db.ExecContext(ctx, `UPDATE site_settings SET site_name=?, primary_color=?, filing_enabled=?, filing_text=?, md5_enabled=?, login_logo_asset_id=NULLIF(?,0), nav_logo_asset_id=NULLIF(?,0), favicon_asset_id=NULLIF(?,0), updated_at=? WHERE id=1`, s.SiteName, s.PrimaryColor, boolInt(s.FilingEnabled), s.FilingText, boolInt(s.MD5Enabled), s.LoginLogoAssetID, s.NavLogoAssetID, s.FaviconAssetID, unixNano(time.Now().UTC()))
	if err != nil {
		return classifyError(err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

// UpdateSiteSettingsWithAudit makes an administrative settings change visible
// only when its corresponding audit event is durable too.
func (r *Repository) UpdateSiteSettingsWithAudit(ctx context.Context, s SiteSettings, event AuditEvent) error {
	if !validSiteSettings(s) {
		return errors.New("invalid site settings")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE site_settings SET site_name=?, primary_color=?, filing_enabled=?, filing_text=?, md5_enabled=?, login_logo_asset_id=NULLIF(?,0), nav_logo_asset_id=NULLIF(?,0), favicon_asset_id=NULLIF(?,0), updated_at=? WHERE id=1`, s.SiteName, s.PrimaryColor, boolInt(s.FilingEnabled), s.FilingText, boolInt(s.MD5Enabled), s.LoginLogoAssetID, s.NavLogoAssetID, s.FaviconAssetID, unixNano(time.Now().UTC()))
	if err != nil {
		return classifyError(err)
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrNotFound
	}
	if err := createAuditEventTx(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit()
}
func validAssetKind(kind string) bool {
	return kind == "login_logo" || kind == "nav_logo" || kind == "favicon"
}
func (r *Repository) ReplaceSiteAsset(ctx context.Context, kind string, a SiteAsset) (SiteAsset, error) {
	return r.replaceSiteAsset(ctx, kind, a, nil)
}

// ReplaceSiteAssetWithAudit atomically publishes an asset and records its
// audit event. The previous unreferenced file is queued for safe cleanup.
func (r *Repository) ReplaceSiteAssetWithAudit(ctx context.Context, kind string, a SiteAsset, event AuditEvent) (SiteAsset, error) {
	return r.replaceSiteAsset(ctx, kind, a, &event)
}

// ResetSiteAssetWithAudit restores the built-in branding fallback by removing
// the configured custom asset, queuing its opaque file for durable cleanup,
// and recording the administrative action in the same transaction.
func (r *Repository) ResetSiteAssetWithAudit(ctx context.Context, kind string, event AuditEvent) error {
	if !validAssetKind(kind) {
		return errors.New("invalid site asset")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	column := map[string]string{"login_logo": "login_logo_asset_id", "nav_logo": "nav_logo_asset_id", "favicon": "favicon_asset_id"}[kind]
	var old sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT `+column+` FROM site_settings WHERE id=1`).Scan(&old); err != nil {
		return classifyError(err)
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE site_settings SET `+column+`=NULL,updated_at=? WHERE id=1`, unixNano(now)); err != nil {
		return err
	}
	if old.Valid {
		var key string
		if err := tx.QueryRowContext(ctx, `SELECT opaque_key FROM site_assets WHERE id=?`, old.Int64).Scan(&key); err != nil {
			return classifyError(err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM site_assets WHERE id=?`, old.Int64); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO site_asset_cleanup(opaque_key,created_at) VALUES(?,?)`, key, unixNano(now)); err != nil {
			return err
		}
	}
	if err := createAuditEventTx(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit()
}
func (r *Repository) replaceSiteAsset(ctx context.Context, kind string, a SiteAsset, event *AuditEvent) (SiteAsset, error) {
	if !validAssetKind(kind) || a.OpaqueKey == "" || a.ContentType == "" || a.Size < 0 || a.Width <= 0 || a.Height <= 0 {
		return SiteAsset{}, errors.New("invalid site asset")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return SiteAsset{}, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `INSERT INTO site_assets(kind,opaque_key,content_type,size,width,height,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, kind, a.OpaqueKey, a.ContentType, a.Size, a.Width, a.Height, unixNano(now), unixNano(now))
	if err != nil {
		return SiteAsset{}, classifyError(err)
	}
	a.ID, err = result.LastInsertId()
	if err != nil {
		return SiteAsset{}, err
	}
	a.Kind, a.CreatedAt, a.UpdatedAt = kind, now, now
	column := map[string]string{"login_logo": "login_logo_asset_id", "nav_logo": "nav_logo_asset_id", "favicon": "favicon_asset_id"}[kind]
	var old sql.NullInt64
	if err = tx.QueryRowContext(ctx, `SELECT `+column+` FROM site_settings WHERE id=1`).Scan(&old); err != nil {
		return SiteAsset{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE site_settings SET `+column+`=?,updated_at=? WHERE id=1`, a.ID, unixNano(now)); err != nil {
		return SiteAsset{}, err
	}
	if old.Valid {
		var oldKey string
		if err = tx.QueryRowContext(ctx, `SELECT opaque_key FROM site_assets WHERE id=?`, old.Int64).Scan(&oldKey); err != nil {
			return SiteAsset{}, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM site_assets WHERE id=?`, old.Int64); err != nil {
			return SiteAsset{}, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO site_asset_cleanup(opaque_key,created_at) VALUES(?,?)`, oldKey, unixNano(now)); err != nil {
			return SiteAsset{}, err
		}
	}
	if event != nil {
		if err = createAuditEventTx(ctx, tx, *event); err != nil {
			return SiteAsset{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return SiteAsset{}, err
	}
	return a, nil
}

// SiteAssetCleanupKeys returns a bounded set of no-longer-referenced files.
func (r *Repository) SiteAssetCleanupKeys(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT opaque_key FROM site_asset_cleanup ORDER BY created_at,opaque_key LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}
func (r *Repository) CompleteSiteAssetCleanup(ctx context.Context, key string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM site_asset_cleanup WHERE opaque_key=?`, key)
	return err
}
func (r *Repository) DeleteSiteAsset(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM site_assets WHERE id=?`, id)
	return err
}
func (r *Repository) PublicSiteAssetByID(ctx context.Context, id int64) (SiteAsset, error) {
	var a SiteAsset
	var c, u int64
	err := r.db.QueryRowContext(ctx, `SELECT id,kind,opaque_key,content_type,size,width,height,created_at,updated_at FROM site_assets WHERE id=?`, id).Scan(&a.ID, &a.Kind, &a.OpaqueKey, &a.ContentType, &a.Size, &a.Width, &a.Height, &c, &u)
	if err != nil {
		return SiteAsset{}, classifyError(err)
	}
	a.CreatedAt, a.UpdatedAt = fromUnixNano(c), fromUnixNano(u)
	return a, nil
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

// UserByID returns one user for management authorization and mutation.
func (r *Repository) UserByID(ctx context.Context, id int64) (User, error) {
	row := r.db.QueryRowContext(ctx, `SELECT id, username, password_hash, disabled, created_at, updated_at FROM users WHERE id = ?`, id)
	return scanUser(row)
}

// IsBootstrapAdmin reports whether userID is the explicitly recorded initial
// administrator. It never infers authority from a user ID.
func (r *Repository) IsBootstrapAdmin(ctx context.Context, userID int64) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM bootstrap_admin WHERE id=1 AND user_id=?)`, userID).Scan(&exists)
	return exists, err
}

// BootstrapAdmin returns the explicitly recorded initial administrator.
func (r *Repository) BootstrapAdmin(ctx context.Context) (User, error) {
	row := r.db.QueryRowContext(ctx, `SELECT u.id, u.username, u.password_hash, u.disabled, u.created_at, u.updated_at FROM users u JOIN bootstrap_admin b ON b.user_id=u.id WHERE b.id=1`)
	return scanUser(row)
}

// SetBootstrapAdmin records a newly-created user as the singleton bootstrap
// administrator. The record is immutable through normal administration APIs.
func (r *Repository) SetBootstrapAdmin(ctx context.Context, userID int64) error {
	now := unixNano(time.Now().UTC())
	_, err := r.db.ExecContext(ctx, `INSERT INTO bootstrap_admin(id,user_id,created_at,updated_at) VALUES(1,?,?,?)`, userID, now, now)
	return classifyError(err)
}

// AdoptLegacyBootstrapAdmin explicitly records the only identity shape emitted
// by pre-role SafeFileHub bootstrap: sfh- plus 24 random bytes encoded with
// base64.RawURLEncoding. Callers must validate the name before invoking this;
// it intentionally never adopts an arbitrary first user.
func (r *Repository) AdoptLegacyBootstrapAdmin(ctx context.Context, userID int64) error {
	return r.SetBootstrapAdmin(ctx, userID)
}

// CreateBootstrapAdmin creates the user and records its bootstrap role in one
// transaction, preventing an unmarked user if process startup is interrupted.
func (r *Repository) CreateBootstrapAdmin(ctx context.Context, user User) (User, error) {
	createdAt := utcOrNow(user.CreatedAt)
	updatedAt := utcOrNow(user.UpdatedAt)
	if user.UpdatedAt.IsZero() {
		updatedAt = createdAt
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO users (username,password_hash,disabled,created_at,updated_at) VALUES(?,?,?,?,?)`, user.Username, user.PasswordHash, boolInt(user.Disabled), unixNano(createdAt), unixNano(updatedAt))
	if err != nil {
		return User{}, classifyError(err)
	}
	user.ID, err = result.LastInsertId()
	if err != nil {
		return User{}, fmt.Errorf("get bootstrap admin id: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO bootstrap_admin(id,user_id,created_at,updated_at) VALUES(1,?,?,?)`, user.ID, unixNano(createdAt), unixNano(updatedAt)); err != nil {
		return User{}, classifyError(err)
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	user.CreatedAt, user.UpdatedAt = createdAt, updatedAt
	return user, nil
}

// UpdateUserCredentials changes password material and/or disabled state without
// exposing the resulting hash to callers. It is deliberately conditional so a
// missing target cannot be mistaken for a successful management operation.
func (r *Repository) UpdateUserCredentials(ctx context.Context, id int64, passwordHash string, disabled bool) error {
	result, err := r.db.ExecContext(ctx, `UPDATE users SET password_hash = ?, disabled = ?, updated_at = ? WHERE id = ?`, passwordHash, boolInt(disabled), unixNano(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("update user credentials: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("confirm user credentials update: %w", err)
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

// ResetInitialAdmin atomically replaces only the explicitly recorded bootstrap
// administrator credentials and identity.
func (r *Repository) ResetInitialAdmin(ctx context.Context, username, passwordHash string) error {
	result, err := r.db.ExecContext(ctx, `UPDATE users SET username=?, password_hash=?, disabled=0, updated_at=? WHERE id=(SELECT user_id FROM bootstrap_admin WHERE id=1)`, username, passwordHash, unixNano(time.Now().UTC()))
	if err != nil {
		return classifyError(err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
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

// FileByID returns a single completed-file metadata row by its opaque API ID.
func (r *Repository) FileByID(ctx context.Context, id int64) (File, error) {
	var file File
	var createdAt, updatedAt int64
	err := r.db.QueryRowContext(ctx, `SELECT id, root_id, logical_path, object_key, size, created_by_user_id, created_at, updated_at, md5_status, md5_digest, md5_error FROM files WHERE id = ? AND delete_state = 'active'`, id).Scan(&file.ID, &file.RootID, &file.LogicalPath, &file.ObjectKey, &file.Size, &file.CreatedByUserID, &createdAt, &updatedAt, &file.MD5Status, &file.MD5Digest, &file.MD5Error)
	if err != nil {
		return File{}, classifyError(err)
	}
	file.CreatedAt, file.UpdatedAt = fromUnixNano(createdAt), fromUnixNano(updatedAt)
	return file, nil
}

// FileForDeletion includes tombstones only for recovery. Readers use FileByID.
func (r *Repository) FileForDeletion(ctx context.Context, id int64) (File, error) {
	var f File
	var created, updated int64
	err := r.db.QueryRowContext(ctx, `SELECT id, root_id, logical_path, object_key, size, created_by_user_id, created_at, updated_at FROM files WHERE id = ?`, id).Scan(&f.ID, &f.RootID, &f.LogicalPath, &f.ObjectKey, &f.Size, &f.CreatedByUserID, &created, &updated)
	if err != nil {
		return File{}, classifyError(err)
	}
	f.CreatedAt, f.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
	return f, nil
}

func (r *Repository) FileByRootAndPath(ctx context.Context, rootID int64, logicalPath string) (File, error) {
	var file File
	var createdAt, updatedAt int64
	err := r.db.QueryRowContext(ctx, `SELECT id, root_id, logical_path, object_key, size, created_by_user_id, created_at, updated_at, md5_status, md5_digest, md5_error FROM files WHERE root_id = ? AND logical_path = ? AND delete_state = 'active'`, rootID, logicalPath).Scan(&file.ID, &file.RootID, &file.LogicalPath, &file.ObjectKey, &file.Size, &file.CreatedByUserID, &createdAt, &updatedAt, &file.MD5Status, &file.MD5Digest, &file.MD5Error)
	if err != nil {
		return File{}, classifyError(err)
	}
	file.CreatedAt, file.UpdatedAt = fromUnixNano(createdAt), fromUnixNano(updatedAt)
	return file, nil
}

// BeginFileDelete tombstones the exact id/key pair before object deletion.
func (r *Repository) BeginFileDelete(ctx context.Context, id int64, key string) error {
	result, err := r.db.ExecContext(ctx, `UPDATE files SET delete_state='deleting', updated_at=? WHERE id=? AND object_key=? AND delete_state='active'`, unixNano(time.Now().UTC()), id, key)
	if err != nil {
		return fmt.Errorf("tombstone file: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}
func (r *Repository) FinalizeFileDelete(ctx context.Context, id int64, key string) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM files WHERE id=? AND object_key=? AND delete_state='deleting'`, id, key)
	if err != nil {
		return fmt.Errorf("finalize file delete: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

// EnqueueObjectCleanup durably remembers failed publication compensation.
func (r *Repository) EnqueueObjectCleanup(ctx context.Context, key, reason string) error {
	now := unixNano(time.Now().UTC())
	_, err := r.db.ExecContext(ctx, `INSERT INTO object_cleanup_jobs(object_key,reason,created_at,updated_at) VALUES(?,?,?,?) ON CONFLICT(object_key) DO UPDATE SET updated_at=excluded.updated_at`, key, reason, now, now)
	return err
}
func (r *Repository) CompleteObjectCleanup(ctx context.Context, key string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM object_cleanup_jobs WHERE object_key=?`, key)
	return err
}

// ObjectCleanupJobs and DeletingFiles are bounded startup-maintenance snapshots.
func (r *Repository) ObjectCleanupJobs(ctx context.Context, limit int) ([]ObjectCleanupJob, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT object_key, reason FROM object_cleanup_jobs ORDER BY created_at LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ObjectCleanupJob
	for rows.Next() {
		var j ObjectCleanupJob
		if err := rows.Scan(&j.ObjectKey, &j.Reason); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func (r *Repository) DeletingFiles(ctx context.Context, limit int) ([]File, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, root_id, logical_path, object_key, size, created_by_user_id, created_at, updated_at FROM files WHERE delete_state='deleting' ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		var f File
		var c, u int64
		if err := rows.Scan(&f.ID, &f.RootID, &f.LogicalPath, &f.ObjectKey, &f.Size, &f.CreatedByUserID, &c, &u); err != nil {
			return nil, err
		}
		f.CreatedAt, f.UpdatedAt = fromUnixNano(c), fromUnixNano(u)
		out = append(out, f)
	}
	return out, rows.Err()
}

// DirectoryByRootAndPath finds one explicitly-created directory. The root
// itself is represented by StorageRoot rather than a directories row.
func (r *Repository) DirectoryByRootAndPath(ctx context.Context, rootID int64, logicalPath string) (Directory, error) {
	var d Directory
	var createdAt, updatedAt int64
	err := r.db.QueryRowContext(ctx, `SELECT id, root_id, logical_path, created_by_user_id, created_at, updated_at FROM directories WHERE root_id = ? AND logical_path = ?`, rootID, logicalPath).Scan(&d.ID, &d.RootID, &d.LogicalPath, &d.CreatedByUserID, &createdAt, &updatedAt)
	if err != nil {
		return Directory{}, classifyError(err)
	}
	d.CreatedAt, d.UpdatedAt = fromUnixNano(createdAt), fromUnixNano(updatedAt)
	return d, nil
}

// DeleteFile deletes exactly one published-file metadata row. The service removes
// the opaque object first, avoiding dangling database references.
func (r *Repository) DeleteFile(ctx context.Context, id int64) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete file: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("confirm file delete: %w", err)
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}
func (r *Repository) CreateDirectory(ctx context.Context, d Directory) (Directory, error) {
	created, updated := utcOrNow(d.CreatedAt), utcOrNow(d.UpdatedAt)
	if d.UpdatedAt.IsZero() {
		updated = created
	}
	result, err := r.db.ExecContext(ctx, `INSERT INTO directories (root_id, logical_path, created_by_user_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`, d.RootID, d.LogicalPath, d.CreatedByUserID, unixNano(created), unixNano(updated))
	if err != nil {
		return Directory{}, classifyError(err)
	}
	d.ID, err = result.LastInsertId()
	if err != nil {
		return Directory{}, fmt.Errorf("get created directory id: %w", err)
	}
	d.CreatedAt, d.UpdatedAt = created, updated
	return d, nil
}
func (r *Repository) DirectoryByID(ctx context.Context, id int64) (Directory, error) {
	var d Directory
	var created, updated int64
	err := r.db.QueryRowContext(ctx, `SELECT id, root_id, logical_path, created_by_user_id, created_at, updated_at FROM directories WHERE id = ?`, id).Scan(&d.ID, &d.RootID, &d.LogicalPath, &d.CreatedByUserID, &created, &updated)
	if err != nil {
		return Directory{}, classifyError(err)
	}
	d.CreatedAt, d.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
	return d, nil
}
func (r *Repository) DirectoryEmpty(ctx context.Context, d Directory) (bool, error) {
	prefix := d.LogicalPath + "/"
	var exists bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM files WHERE root_id=? AND substr(logical_path, 1, length(?)) = ? UNION ALL SELECT 1 FROM directories WHERE root_id=? AND substr(logical_path, 1, length(?)) = ?)`, d.RootID, prefix, prefix, d.RootID, prefix, prefix).Scan(&exists)
	return !exists, err
}
func (r *Repository) DeleteDirectory(ctx context.Context, id int64) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM directories WHERE id = ?`, id)
	if err != nil {
		return classifyError(err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("confirm directory delete: %w", err)
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
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
	if session.Status == "" {
		session.Status = "active"
	}
	result, err := r.db.ExecContext(ctx, `INSERT INTO upload_sessions (id, user_id, root_id, logical_path, staging_path, offset, length, status, expires_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, session.ID, session.UserID, session.RootID, session.LogicalPath, session.StagingPath, session.Offset, session.Length, session.Status, unixNano(expiresAt), unixNano(createdAt), unixNano(updatedAt))
	if err != nil {
		return UploadSession{}, classifyError(err)
	}
	if _, err := result.RowsAffected(); err != nil {
		return UploadSession{}, fmt.Errorf("confirm upload session creation: %w", err)
	}
	session.CreatedAt, session.UpdatedAt, session.ExpiresAt = createdAt, updatedAt, expiresAt
	return session, nil
}

// UploadSessions returns a bounded, deterministic maintenance snapshot. Callers
// must re-read each row under its lifecycle lock before acting on it.
func (r *Repository) UploadSessions(ctx context.Context, limit int) ([]UploadSession, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, user_id, root_id, logical_path, staging_path, offset, length, status, expires_at, created_at, updated_at FROM upload_sessions WHERE status IN ('active', 'cancelled', 'cleanup_pending') ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query upload sessions: %w", err)
	}
	defer rows.Close()
	sessions := make([]UploadSession, 0, limit)
	for rows.Next() {
		var s UploadSession
		var expiresAt, createdAt, updatedAt int64
		if err := rows.Scan(&s.ID, &s.UserID, &s.RootID, &s.LogicalPath, &s.StagingPath, &s.Offset, &s.Length, &s.Status, &expiresAt, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan upload session: %w", err)
		}
		s.ExpiresAt, s.CreatedAt, s.UpdatedAt = fromUnixNano(expiresAt), fromUnixNano(createdAt), fromUnixNano(updatedAt)
		sessions = append(sessions, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate upload sessions: %w", err)
	}
	return sessions, nil
}

func (r *Repository) UploadSessionByID(ctx context.Context, id string) (UploadSession, error) {
	var session UploadSession
	var expiresAt, createdAt, updatedAt int64
	err := r.db.QueryRowContext(ctx, `SELECT id, user_id, root_id, logical_path, staging_path, offset, length, status, expires_at, created_at, updated_at FROM upload_sessions WHERE id = ?`, id).Scan(&session.ID, &session.UserID, &session.RootID, &session.LogicalPath, &session.StagingPath, &session.Offset, &session.Length, &session.Status, &expiresAt, &createdAt, &updatedAt)
	if err != nil {
		return UploadSession{}, classifyError(err)
	}
	session.ExpiresAt, session.CreatedAt, session.UpdatedAt = fromUnixNano(expiresAt), fromUnixNano(createdAt), fromUnixNano(updatedAt)
	return session, nil
}

func (r *Repository) UpdateUploadOffset(ctx context.Context, id string, expected, offset int64) error {
	result, err := r.db.ExecContext(ctx, `UPDATE upload_sessions SET offset = ?, updated_at = ? WHERE id = ? AND offset = ?`, offset, unixNano(time.Now().UTC()), id, expected)
	if err != nil {
		return fmt.Errorf("update upload offset: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("confirm upload offset: %w", err)
	}
	if n != 1 {
		return ErrConflict
	}
	return nil
}

// UpdateUploadStatus performs a lifecycle state transition atomically. The
// caller holds the staging lifecycle flock while using it.
func (r *Repository) UpdateUploadStatus(ctx context.Context, id, expected, status string) error {
	result, err := r.db.ExecContext(ctx, `UPDATE upload_sessions SET status = ?, updated_at = ? WHERE id = ? AND status = ?`, status, unixNano(time.Now().UTC()), id, expected)
	if err != nil {
		return fmt.Errorf("update upload status: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("confirm upload status: %w", err)
	}
	if n != 1 {
		return ErrConflict
	}
	return nil
}

// CompleteUpload records the published object and deletes exactly its active
// session atomically. Publication has already happened, so metadata can never
// point at a nonexistent object.
func (r *Repository) CompleteUpload(ctx context.Context, file File, sessionID string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin complete upload: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM upload_sessions WHERE id = ? AND status = 'active' AND expires_at > ?`, sessionID, unixNano(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("delete completed upload session: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("confirm completed upload session: %w", err)
	}
	if n != 1 {
		return ErrConflict
	}
	created := utcOrNow(file.CreatedAt)
	updated := utcOrNow(file.UpdatedAt)
	if file.UpdatedAt.IsZero() {
		updated = created
	}
	var md5Enabled int
	if err = tx.QueryRowContext(ctx, `SELECT md5_enabled FROM site_settings WHERE id=1`).Scan(&md5Enabled); err != nil {
		return err
	}
	status := MD5Disabled
	if md5Enabled != 0 {
		status = MD5Pending
	}
	result, err = tx.ExecContext(ctx, `INSERT INTO files (root_id, logical_path, object_key, size, created_by_user_id, created_at, updated_at, md5_status) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, file.RootID, file.LogicalPath, file.ObjectKey, file.Size, file.CreatedByUserID, unixNano(created), unixNano(updated), status)
	if err != nil {
		return classifyError(err)
	}
	if md5Enabled != 0 {
		id, e := result.LastInsertId()
		if e != nil {
			return e
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO md5_tasks(file_id,status,attempts,max_attempts,available_at,created_at,updated_at) VALUES (?,'pending',0,3,?,?,?)`, id, unixNano(created), unixNano(created), unixNano(updated)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit complete upload: %w", err)
	}
	return nil
}

func (r *Repository) DeleteUploadSession(ctx context.Context, id string) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM upload_sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete upload session: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("confirm upload session delete: %w", err)
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

func (r *Repository) CreateAuditEvent(ctx context.Context, event AuditEvent) (AuditEvent, error) {
	event, err := prepareAuditEvent(event)
	if err != nil {
		return AuditEvent{}, err
	}
	result, err := r.db.ExecContext(ctx, `INSERT INTO audit_events (user_id, root_id, action, logical_path, detail, status, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, event.UserID, nullableRootID(event.RootID), event.Action, event.LogicalPath, event.Detail, event.Status, unixNano(event.CreatedAt))
	if err != nil {
		return AuditEvent{}, classifyError(err)
	}
	event.ID, err = result.LastInsertId()
	if err != nil {
		return AuditEvent{}, fmt.Errorf("get created audit event id: %w", err)
	}
	return event, nil
}
func prepareAuditEvent(event AuditEvent) (AuditEvent, error) {
	event.Detail = redactAuditDetail(event.Detail)
	if event.Status == 0 {
		event.Status = auditStatus(event.Detail)
	}
	event.CreatedAt = utcOrNow(event.CreatedAt)
	return event, nil
}
func nullableRootID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
func createAuditEventTx(ctx context.Context, tx *sql.Tx, event AuditEvent) error {
	event, err := prepareAuditEvent(event)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_events (user_id, root_id, action, logical_path, detail, status, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, event.UserID, nullableRootID(event.RootID), event.Action, event.LogicalPath, event.Detail, event.Status, unixNano(event.CreatedAt))
	if err != nil {
		return classifyError(err)
	}
	return nil
}

// redactAuditDetail preserves only non-sensitive operational metadata. Audit
// records never retain supplied credentials, request content, or tokens.
func redactAuditDetail(detail string) string {
	fields := strings.Fields(detail)
	kept := make([]string, 0, len(fields))
	for _, field := range fields {
		lower := strings.ToLower(field)
		if strings.HasPrefix(lower, "password=") || strings.HasPrefix(lower, "content=") || strings.HasPrefix(lower, "token=") || strings.HasPrefix(lower, "secret=") || strings.HasPrefix(lower, "authorization=") {
			continue
		}
		if strings.HasPrefix(lower, "status=") || strings.HasPrefix(lower, "target_user_id=") || strings.HasPrefix(lower, "request_id=") || strings.HasPrefix(lower, "session_audit_id=") || strings.HasPrefix(lower, "result=") {
			kept = append(kept, field)
		}
	}
	return strings.Join(kept, " ")
}

func auditStatus(detail string) int {
	for _, field := range strings.Fields(detail) {
		if strings.HasPrefix(strings.ToLower(field), "status=") {
			var status int
			if _, err := fmt.Sscanf(field, "status=%d", &status); err == nil {
				return status
			}
		}
	}
	return 0
}

func (r *Repository) AuditEventsForUser(ctx context.Context, userID int64) ([]AuditEvent, error) {
	return r.auditEvents(ctx, `SELECT id, user_id, root_id, action, logical_path, detail, status, created_at FROM audit_events WHERE user_id = ? ORDER BY id`, userID)
}

// AuditEvents returns a bounded audit history for authorized administrative
// visibility. Callers must enforce administration authorization before use.
func (r *Repository) AuditEvents(ctx context.Context, limit int) ([]AuditEvent, error) {
	if limit <= 0 {
		return nil, nil
	}
	return r.auditEvents(ctx, `SELECT id, user_id, root_id, action, logical_path, detail, status, created_at FROM audit_events ORDER BY id DESC LIMIT ?`, limit)
}

func (r *Repository) auditEvents(ctx context.Context, query string, args ...any) ([]AuditEvent, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query audit events: %w", err)
	}
	defer rows.Close()
	var events []AuditEvent
	for rows.Next() {
		var event AuditEvent
		var rootID sql.NullInt64
		var createdAt int64
		if err := rows.Scan(&event.ID, &event.UserID, &rootID, &event.Action, &event.LogicalPath, &event.Detail, &event.Status, &createdAt); err != nil {
			return nil, fmt.Errorf("scan audit event: %w", err)
		}
		event.RootID = rootID.Int64
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
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "unique constraint") || strings.Contains(message, "constraint failed") ||
		strings.Contains(message, "published path occupied") || strings.Contains(message, "directory not empty") ||
		strings.Contains(message, "parent directory not found") {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return err
}

// ClaimMD5Task atomically reserves one due task. The single UPDATE predicate
// makes a task unavailable to every other SQLite connection before it is read.
func (r *Repository) ClaimMD5Task(ctx context.Context) (MD5Task, error) {
	now := unixNano(time.Now().UTC())
	row := r.db.QueryRowContext(ctx, `UPDATE md5_tasks SET status='computing', attempts=attempts+1, claimed_at=?, updated_at=? WHERE file_id=(SELECT file_id FROM md5_tasks WHERE status='pending' AND available_at<=? ORDER BY available_at,file_id LIMIT 1) AND status='pending' RETURNING file_id,status,attempts,max_attempts,available_at,claimed_at`, now, now, now)
	var t MD5Task
	var available, claimed sql.NullInt64
	if err := row.Scan(&t.FileID, &t.Status, &t.Attempts, &t.MaxAttempts, &available, &claimed); err != nil {
		return MD5Task{}, classifyError(err)
	}
	t.AvailableAt = fromUnixNano(available.Int64)
	if claimed.Valid {
		t.ClaimedAt = fromUnixNano(claimed.Int64)
	}
	return t, nil
}
func (r *Repository) CompleteMD5Task(ctx context.Context, fileID int64, digest string) error {
	if len(digest) != 32 {
		return errors.New("invalid md5 digest")
	}
	for _, c := range digest {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return errors.New("invalid md5 digest")
		}
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE md5_tasks SET status='complete',updated_at=? WHERE file_id=? AND status='computing'`, unixNano(time.Now().UTC()), fileID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	if _, err = tx.ExecContext(ctx, `UPDATE files SET md5_status='ready',md5_digest=?,md5_error='',updated_at=? WHERE id=?`, digest, unixNano(time.Now().UTC()), fileID); err != nil {
		return err
	}
	return tx.Commit()
}
func (r *Repository) FailMD5Task(ctx context.Context, fileID int64, message string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := unixNano(time.Now().UTC())
	result, err := tx.ExecContext(ctx, `UPDATE md5_tasks SET status=CASE WHEN attempts>=max_attempts THEN 'failed' ELSE 'pending' END, available_at=?, claimed_at=NULL, updated_at=? WHERE file_id=? AND status='computing'`, now, now, fileID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	if _, err = tx.ExecContext(ctx, `UPDATE files SET md5_status=CASE WHEN (SELECT status FROM md5_tasks WHERE file_id=?)='failed' THEN 'failed' ELSE 'pending' END,md5_error=?,updated_at=? WHERE id=?`, fileID, message, now, fileID); err != nil {
		return err
	}
	return tx.Commit()
}

// RequeueComputingMD5Tasks recovers at most limit computing claims whose lease
// expired before staleBefore. It is safe to run repeatedly during normal work.
func (r *Repository) RequeueComputingMD5Tasks(ctx context.Context, limit int, staleBefore time.Time) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	now := unixNano(time.Now().UTC())
	result, err := r.db.ExecContext(ctx, `UPDATE md5_tasks SET status='pending',claimed_at=NULL,available_at=?,updated_at=? WHERE file_id IN (SELECT file_id FROM md5_tasks WHERE status='computing' AND claimed_at<=? ORDER BY claimed_at,file_id LIMIT ?)`, now, now, unixNano(staleBefore), limit)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}
