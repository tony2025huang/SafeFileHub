// Package upload implements persistent, bounded-memory resumable staging writes.
package upload

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/storage"
)

var ErrOffset = errors.New("upload offset conflict")
var ErrTooLarge = errors.New("upload exceeds declared length")
var ErrIncomplete = errors.New("upload is incomplete")
var ErrChecksum = errors.New("upload checksum mismatch")

type Session struct {
	ID             string
	Offset, Length int64
	ExpiresAt      time.Time
}
type repository interface {
	CreateUploadSession(context.Context, db.UploadSession) (db.UploadSession, error)
	UploadSessionByID(context.Context, string) (db.UploadSession, error)
	UpdateUploadOffset(context.Context, string, int64, int64) error
	UpdateUploadStatus(context.Context, string, string, string) error
	DeleteUploadSession(context.Context, string) error
	CompleteUpload(context.Context, db.File, string) error
}
type Manager struct {
	repo  repository
	store *storage.ObjectStore
	chunk int64
	ttl   time.Duration
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func New(repo repository, store *storage.ObjectStore, chunk int64, ttl time.Duration) *Manager {
	return &Manager{repo: repo, store: store, chunk: chunk, ttl: ttl, locks: map[string]*sync.Mutex{}}
}
func (m *Manager) Create(ctx context.Context, userID, rootID int64, path string, length int64) (Session, error) {
	if length < 0 {
		return Session{}, ErrTooLarge
	}
	id, err := newID()
	if err != nil {
		return Session{}, err
	}
	name := "staging/" + id + ".part"
	f, err := m.store.CreateStaging(name)
	if err != nil {
		return Session{}, err
	}
	if err = f.Close(); err != nil {
		return Session{}, err
	}
	s, err := m.repo.CreateUploadSession(ctx, db.UploadSession{ID: id, UserID: userID, RootID: rootID, LogicalPath: path, StagingPath: name, Length: length, Status: "active", ExpiresAt: time.Now().Add(m.ttl)})
	if err != nil {
		_ = m.store.RemoveStaging(name)
		return Session{}, err
	}
	return public(s), nil
}
func (m *Manager) Get(ctx context.Context, id string) (db.UploadSession, error) {
	s, err := m.repo.UploadSessionByID(ctx, id)
	if err != nil {
		return s, err
	}
	if s.Status != "active" || !s.ExpiresAt.After(time.Now()) {
		_ = m.cleanup(ctx, id, true)
		return db.UploadSession{}, db.ErrNotFound
	}
	return s, nil
}

// withLifecycleLock obtains the flock from a pathname lookup, then re-reads
// metadata after locking. A changed pathname is never acted on from a stale snapshot.
func (m *Manager) withLifecycleLock(ctx context.Context, id string, fn func(db.UploadSession) error) error {
	local := m.lock(id)
	local.Lock()
	defer local.Unlock()
	for {
		before, err := m.repo.UploadSessionByID(ctx, id)
		if err != nil {
			return err
		}
		f, err := m.store.LockStagingLifecycle(before.StagingPath)
		if err != nil {
			return err
		}
		s, readErr := m.repo.UploadSessionByID(ctx, id)
		if readErr != nil {
			_ = m.store.UnlockStagingLifecycle(f)
			return readErr
		}
		if s.StagingPath != before.StagingPath {
			_ = m.store.UnlockStagingLifecycle(f)
			continue
		}
		err = fn(s)
		unlockErr := m.store.UnlockStagingLifecycle(f)
		if err != nil {
			return err
		}
		return unlockErr
	}
}
func (m *Manager) Write(ctx context.Context, id string, offset int64, body io.Reader) (next int64, err error) {
	err = m.withLifecycleLock(ctx, id, func(s db.UploadSession) error {
		f, err := m.store.OpenStagingWrite(s.StagingPath)
		if err != nil {
			return err
		}
		defer f.Close()
		if s.Status != "active" || !s.ExpiresAt.After(time.Now()) {
			_ = m.cleanupLocked(ctx, s)
			return db.ErrNotFound
		}
		if offset != s.Offset {
			next = s.Offset
			return ErrOffset
		}
		if err := f.Truncate(s.Offset); err != nil {
			return fmt.Errorf("normalize staging: %w", err)
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return err
		}
		n, copyErr := io.Copy(f, io.LimitReader(&contextReader{ctx: ctx, r: body}, s.Length-offset+1))
		if copyErr != nil {
			_ = f.Truncate(s.Offset)
			return copyErr
		}
		if n > s.Length-offset {
			_ = f.Truncate(s.Offset)
			return ErrTooLarge
		}
		if err := f.Sync(); err != nil {
			_ = f.Truncate(s.Offset)
			return fmt.Errorf("sync staging: %w", err)
		}
		if err := m.repo.UpdateUploadOffset(ctx, id, s.Offset, s.Offset+n); err != nil {
			_ = f.Truncate(s.Offset)
			return err
		}
		next = s.Offset + n
		return nil
	})
	return next, err
}

// Complete validates the immutable staged inode, publishes it atomically, then
// commits metadata and session deletion in one short compare-and-swap DB transaction.
func (m *Manager) Complete(ctx context.Context, id, expectedSHA256 string) error {
	return m.withLifecycleLock(ctx, id, func(s db.UploadSession) error {
		if s.Status != "active" || !s.ExpiresAt.After(time.Now()) {
			return db.ErrNotFound
		}
		if s.Offset != s.Length {
			return ErrIncomplete
		}
		n, sum, err := m.store.HashAndSyncStaging(s.StagingPath)
		if err != nil {
			return err
		}
		if n != s.Length {
			return ErrIncomplete
		}
		if expectedSHA256 != "" {
			want, err := hex.DecodeString(expectedSHA256)
			if err != nil || len(want) != sha256.Size || !equalHash(sum[:], want) {
				return ErrChecksum
			}
		}
		key, err := m.store.PublishStaging(s.StagingPath)
		if err != nil {
			return err
		}
		// Publish precedes metadata. If the DB commit fails, the object is an
		// unreachable orphan; never a listing-visible half completion.
		if err := m.repo.CompleteUpload(ctx, db.File{RootID: s.RootID, LogicalPath: s.LogicalPath, ObjectKey: key, Size: s.Length, CreatedByUserID: s.UserID}, s.ID); err != nil {
			// Do not leave an active row pointing at a vanished part after a DB
			// failure. Roll the atomic publish back while holding its lifecycle lock.
			if restoreErr := m.store.RestorePublished(key, s.StagingPath); restoreErr != nil {
				return fmt.Errorf("complete metadata: %w (restore staging: %v)", err, restoreErr)
			}
			return err
		}
		return nil
	})
}
func equalHash(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func (m *Manager) Cancel(ctx context.Context, id string) error { return m.cleanup(ctx, id, false) }
func (m *Manager) cleanup(ctx context.Context, id string, expiryOnly bool) error {
	return m.withLifecycleLock(ctx, id, func(s db.UploadSession) error {
		// An expiry observer may have read an old row before waiting for flock.
		// Recheck after the lock so it never deletes a now-live active session.
		if expiryOnly && s.Status == "active" && s.ExpiresAt.After(time.Now()) {
			return nil
		}
		return m.cleanupLocked(ctx, s)
	})
}

// cleanupLocked is idempotent: persist intent before unlink, then delete metadata.
// cleanup_pending is retained on any failure, so a later Cancel/Get converges.
func (m *Manager) cleanupLocked(ctx context.Context, s db.UploadSession) error {
	if s.Status == "complete" {
		return db.ErrNotFound
	}
	if s.Status == "active" {
		if err := m.repo.UpdateUploadStatus(ctx, s.ID, "active", "cancelled"); err != nil {
			return err
		}
		s.Status = "cancelled"
	}
	if s.Status == "cancelled" {
		if err := m.repo.UpdateUploadStatus(ctx, s.ID, "cancelled", "cleanup_pending"); err != nil {
			return err
		}
	}
	if err := m.store.RemoveStaging(s.StagingPath); err != nil {
		return err
	}
	if err := m.repo.DeleteUploadSession(ctx, s.ID); err != nil {
		return err
	}
	return nil
}
func (m *Manager) lock(id string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks[id] == nil {
		m.locks[id] = &sync.Mutex{}
	}
	return m.locks[id]
}
func public(s db.UploadSession) Session {
	return Session{ID: s.ID, Offset: s.Offset, Length: s.Length, ExpiresAt: s.ExpiresAt}
}
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate upload id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	default:
		return c.r.Read(p)
	}
}

// RecoveryReport is the bounded outcome of startup/maintenance reconciliation.
type RecoveryReport struct{ Checked, Kept, Cancelled, Orphans int }

// Recover reconciles sessions with their private staging files. Valid live
// sessions survive restarts. Missing or size-inconsistent active parts are
// transitioned through cleanup, never reported as writable. The scan is
// bounded and suitable for a background startup job, not a request handler.
func (m *Manager) Recover(ctx context.Context, limit int, dryRun bool) (RecoveryReport, error) {
	if limit <= 0 || limit > 64 {
		limit = 64
	}
	names, err := m.store.StagingNames(limit)
	if err != nil {
		return RecoveryReport{}, err
	}
	report := RecoveryReport{Checked: len(names)}
	for _, name := range names {
		id := strings.TrimSuffix(strings.TrimPrefix(name, "staging/"), ".part")
		s, err := m.repo.UploadSessionByID(ctx, id)
		if errors.Is(err, db.ErrNotFound) {
			if !dryRun {
				if err := m.store.RemoveStaging(name); err != nil {
					return report, err
				}
			}
			report.Orphans++
			continue
		}
		if err != nil {
			return report, err
		}
		f, err := m.store.OpenStaging(s.StagingPath)
		valid := err == nil
		if valid {
			info, statErr := f.Stat()
			_ = f.Close()
			valid = statErr == nil && info.Mode().IsRegular() && info.Size() == s.Offset
		}
		if s.Status == "active" && s.ExpiresAt.After(time.Now()) && valid {
			report.Kept++
			continue
		}
		if !dryRun {
			if err := m.cleanup(ctx, s.ID, false); err != nil && !errors.Is(err, db.ErrNotFound) {
				return report, err
			}
		}
		report.Cancelled++
	}
	return report, nil
}
