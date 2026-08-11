// Package upload implements persistent, bounded-memory resumable staging writes.
package upload

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/storage"
)

var ErrOffset = errors.New("upload offset conflict")
var ErrTooLarge = errors.New("upload exceeds declared length")

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
