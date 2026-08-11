package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
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
}
type SessionConfig struct {
	CookieName string
	TTL        time.Duration
	Secure     bool
	SameSite   http.SameSite
	Now        func() time.Time
}
type SessionManager struct {
	store  SessionStore
	config SessionConfig
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
	return &SessionManager{store: store, config: config}
}
func (m *SessionManager) Create(ctx context.Context, userID int64) (string, error) {
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
func (m *SessionManager) SetCookie(w http.ResponseWriter, id string) {
	now := m.config.Now().UTC()
	http.SetCookie(w, &http.Cookie{Name: m.config.CookieName, Value: id, Path: "/", Expires: now.Add(m.config.TTL), MaxAge: int(m.config.TTL.Seconds()), Secure: m.config.Secure, HttpOnly: true, SameSite: m.config.SameSite})
}

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
func (s *Service) Authenticate(ctx context.Context, username, password string) (db.User, error) {
	u, err := s.users.UserByUsername(ctx, username)
	if err != nil || u.Disabled || !VerifyPassword(u.PasswordHash, password) {
		return db.User{}, ErrInvalidCredentials
	}
	return u, nil
}
