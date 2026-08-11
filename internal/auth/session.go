package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/example/safefilehub/internal/db"
)

var ErrSessionNotFound = errors.New("session not found")
var ErrSessionExpired = errors.New("session expired")

type Session struct {
	ID        string
	UserID    int64
	ExpiresAt time.Time
}
type SessionStore interface {
	Create(context.Context, Session) error
	Lookup(context.Context, string) (Session, error)
	Delete(context.Context, string) error
	// DeleteExpired removes at most limit sessions expiring at or before now.
	// It is intentionally bounded so login/request paths cannot be used to
	// trigger unbounded maintenance work.
	DeleteExpired(context.Context, time.Time, int) (int, error)
}
type SessionConfig struct {
	CookieName       string
	TTL              time.Duration
	Secure           bool
	SameSite         http.SameSite
	Now              func() time.Time
	LifecycleContext context.Context
	GCDeleteTimeout  time.Duration
}

const sessionGCMaxDeletes = 64
const defaultGCDeleteTimeout = 5 * time.Second

type SessionManager struct {
	store  SessionStore
	config SessionConfig

	lifecycleCtx    context.Context
	cancelLifecycle context.CancelFunc
	closeOnce       sync.Once
	gcMu            sync.Mutex
	gcRunning       bool
	closed          bool
	gcWG            sync.WaitGroup
}

func NewSessionManager(store SessionStore, config SessionConfig) *SessionManager {
	if config.CookieName == "" {
		config.CookieName = "safefilehub_session"
	}
	if config.TTL <= 0 {
		config.TTL = 24 * time.Hour
	}
	if config.SameSite == 0 {
		config.SameSite = http.SameSiteLaxMode
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.GCDeleteTimeout <= 0 {
		config.GCDeleteTimeout = defaultGCDeleteTimeout
	}
	if config.LifecycleContext == nil {
		config.LifecycleContext = context.Background()
	}
	lifecycleCtx, cancelLifecycle := context.WithCancel(config.LifecycleContext)
	return &SessionManager{store: store, config: config, lifecycleCtx: lifecycleCtx, cancelLifecycle: cancelLifecycle}
}
func (m *SessionManager) Create(ctx context.Context, userID int64) (string, error) {
	// Maintenance must never delay authentication. A single background worker
	// drains expired sessions in bounded batches; it deliberately does not use
	// the request context so client cancellation cannot abandon cleanup.
	m.scheduleGC(m.config.Now().UTC())
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(b)
	if err := m.store.Create(ctx, Session{ID: id, UserID: userID, ExpiresAt: m.config.Now().UTC().Add(m.config.TTL)}); err != nil {
		return "", err
	}
	return id, nil
}

// Revoke deletes the supplied server-side session. Session authorization is
// always resolved from this store; the client cookie contains only the opaque ID.
func (m *SessionManager) Revoke(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	return m.store.Delete(ctx, id)
}

// Logout revokes the supplied server-side session.
func (m *SessionManager) Logout(ctx context.Context, id string) error {
	return m.Revoke(ctx, id)
}

func (m *SessionManager) UserID(ctx context.Context, id string) (int64, error) {
	s, err := m.store.Lookup(ctx, id)
	if err != nil {
		return 0, err
	}
	if !m.config.Now().UTC().Before(s.ExpiresAt) {
		_ = m.store.Delete(ctx, id)
		return 0, ErrSessionExpired
	}
	return s.UserID, nil
}

// GC performs one bounded maintenance pass. Callers that need to drain a
// larger backlog can invoke it repeatedly from a maintenance loop.
func (m *SessionManager) GC(ctx context.Context) (int, error) {
	return m.store.DeleteExpired(ctx, m.config.Now().UTC(), sessionGCMaxDeletes)
}

func (m *SessionManager) scheduleGC(now time.Time) {
	m.gcMu.Lock()
	defer m.gcMu.Unlock()
	if m.gcRunning || m.closed {
		return
	}
	m.gcRunning = true
	m.gcWG.Add(1)
	go m.runGC(now)
}

// runGC is the only automatic GC worker. Each pass is bounded, and the
// worker exits as soon as a pass is not full. This both eventually drains a
// backlog and prevents concurrent Create calls from multiplying GC work.
func (m *SessionManager) runGC(now time.Time) {
	defer func() {
		m.gcMu.Lock()
		m.gcRunning = false
		m.gcMu.Unlock()
		m.gcWG.Done()
	}()
	for {
		ctx, cancel := context.WithTimeout(m.lifecycleCtx, m.config.GCDeleteTimeout)
		deleted, err := m.store.DeleteExpired(ctx, now, sessionGCMaxDeletes)
		cancel()
		if err != nil {
			if m.lifecycleCtx.Err() == nil {
				// Authentication remains available; report failed maintenance instead
				// of silently discarding an operational signal.
				log.Printf("auth: session garbage collection failed: %v", err)
			}
			return
		}
		if deleted < sessionGCMaxDeletes {
			return
		}
	}
}

// Close stops the background GC worker and prevents future workers from starting.
func (m *SessionManager) Close() {
	m.closeOnce.Do(func() {
		m.gcMu.Lock()
		m.closed = true
		m.gcMu.Unlock()
		m.cancelLifecycle()
		m.gcWG.Wait()
	})
}

func (m *SessionManager) SetCookie(w http.ResponseWriter, id string) {
	now := m.config.Now().UTC()
	http.SetCookie(w, &http.Cookie{Name: m.config.CookieName, Value: id, Path: "/", Expires: now.Add(m.config.TTL), MaxAge: int(m.config.TTL.Seconds()), Secure: m.config.Secure, HttpOnly: true, SameSite: m.config.SameSite})
}

// ClearSessionCookie invalidates the client cookie using the same security attributes.
func (m *SessionManager) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: m.config.CookieName, Value: "", Path: "/", Expires: m.config.Now().UTC().Add(-time.Second), MaxAge: -1, Secure: m.config.Secure, HttpOnly: true, SameSite: m.config.SameSite})
}

func (m *SessionManager) CookieName() string { return m.config.CookieName }

type memorySessionStore struct {
	mu       sync.Mutex
	sessions map[string]Session
}

func NewMemorySessionStore() SessionStore {
	return &memorySessionStore{sessions: make(map[string]Session)}
}
func (s *memorySessionStore) Create(_ context.Context, v Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[v.ID] = v
	return nil
}
func (s *memorySessionStore) Lookup(_ context.Context, id string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.sessions[id]
	if !ok {
		return Session{}, ErrSessionNotFound
	}
	return v, nil
}
func (s *memorySessionStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	return nil
}
func (s *memorySessionStore) DeleteExpired(_ context.Context, now time.Time, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	deleted := 0
	for id, session := range s.sessions {
		if !session.ExpiresAt.After(now) {
			delete(s.sessions, id)
			deleted++
			if deleted == limit {
				break
			}
		}
	}
	return deleted, nil
}

type Service struct {
	users interface {
		UserByUsername(context.Context, string) (db.User, error)
	}
}

func NewService(users interface {
	UserByUsername(context.Context, string) (db.User, error)
}) *Service {
	return &Service{users: users}
}

// dummyPasswordHash is generated with the fixed verifier parameters and is a
// package constant, never data supplied by the user database.
const dummyPasswordHash = "$argon2id$v=19$m=65536,t=3,p=1$HuMhhGn/0DHkTtaVeqF4Uw$e6aHIED4yL6VDXJ/G63RvTv+IhHE22Kz8RvrvDrkfJ8"

func (s *Service) Authenticate(ctx context.Context, username, password string) (db.User, error) {
	u, err := s.users.UserByUsername(ctx, username)
	if err != nil || u.Disabled {
		// Always perform a bounded, database-independent verification to make
		// absent and disabled accounts indistinguishable from bad credentials.
		_ = VerifyPassword(dummyPasswordHash, password)
		return db.User{}, ErrInvalidCredentials
	}
	if !VerifyPassword(u.PasswordHash, password) {
		return db.User{}, ErrInvalidCredentials
	}
	return u, nil
}
