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
	s, err := m.repo.CreateUploadSession(ctx, db.UploadSession{ID: id, UserID: userID, RootID: rootID, LogicalPath: path, StagingPath: name, Length: length, ExpiresAt: time.Now().Add(m.ttl)})
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
	if !s.ExpiresAt.After(time.Now()) {
		_ = m.Cancel(ctx, id)
		return db.UploadSession{}, db.ErrNotFound
	}
	return s, nil
}
func (m *Manager) Write(ctx context.Context, id string, offset int64, body io.Reader) (int64, error) {
	lock := m.lock(id)
	lock.Lock()
	defer lock.Unlock()
	s, err := m.Get(ctx, id)
	if err != nil {
		return 0, err
	}
	if offset != s.Offset {
		return s.Offset, ErrOffset
	}
	f, err := m.store.OpenStagingWrite(s.StagingPath)
	if err != nil {
		return s.Offset, err
	}
	defer f.Close()
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return s.Offset, err
	}
	n, err := io.Copy(f, io.LimitReader(&contextReader{ctx: ctx, r: body}, s.Length-offset+1))
	if err != nil {
		return s.Offset, err
	}
	if n > s.Length-offset {
		return s.Offset, ErrTooLarge
	}
	if err = m.repo.UpdateUploadOffset(ctx, id, s.Offset, s.Offset+n); err != nil {
		return s.Offset, err
	}
	return s.Offset + n, nil
}
func (m *Manager) Cancel(ctx context.Context, id string) error {
	lock := m.lock(id)
	lock.Lock()
	defer lock.Unlock()
	s, err := m.repo.UploadSessionByID(ctx, id)
	if err != nil {
		return err
	}
	if err = m.repo.DeleteUploadSession(ctx, id); err != nil {
		return err
	}
	return m.store.RemoveStaging(s.StagingPath)
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
