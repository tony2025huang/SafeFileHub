// Package archive creates short-lived ZIP artifacts from authorized logical files.
package archive

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

var (
	ErrForbidden = errors.New("archive permission denied")
	ErrNotFound  = errors.New("archive job not found")
)

type Status string

const (
	Running   Status = "running"
	Complete  Status = "complete"
	Cancelled Status = "cancelled"
	Failed    Status = "failed"
)

type Entry struct {
	LogicalPath, ObjectKey string
	Size                   int64
}
type Source interface {
	Open(context.Context, string) (io.ReadCloser, error)
}
type Authorizer interface {
	Allow(context.Context, string) (bool, error)
}
type Options struct {
	Workers, MaxFiles int
	MaxBytes          int64
	TTL               time.Duration
	TempDir           string
}
type Job struct {
	ID string
	// UserID privately binds the temporary artifact to its creator.
	UserID               int64
	Status               Status
	CreatedAt, ExpiresAt time.Time
	Size                 int64
	Err                  error
}
type task struct {
	id      string
	entries []Entry
	ctx     context.Context
}
type managedJob struct {
	Job
	file   string
	cancel context.CancelFunc
}

// Manager owns a bounded job queue and keeps finished artifacts outside the
// object namespace, so they cannot become regular file-listing candidates.
type Manager struct {
	source  Source
	options Options
	tasks   chan task
	mu      sync.Mutex
	jobs    map[string]*managedJob
	closed  chan struct{}
	once    sync.Once
	wg      sync.WaitGroup
}

func New(options Options, source Source) (*Manager, error) {
	if source == nil || options.Workers <= 0 || options.MaxFiles <= 0 || options.MaxBytes <= 0 || options.TTL <= 0 {
		return nil, errors.New("invalid archive options")
	}
	if options.TempDir == "" {
		return nil, errors.New("archive temp dir is required")
	}
	if err := os.MkdirAll(options.TempDir, 0700); err != nil {
		return nil, err
	}
	m := &Manager{source: source, options: options, tasks: make(chan task, options.Workers), jobs: make(map[string]*managedJob), closed: make(chan struct{})}
	for i := 0; i < options.Workers; i++ {
		m.wg.Add(1)
		go m.worker()
	}
	return m, nil
}
func (m *Manager) Close() {
	m.once.Do(func() {
		close(m.closed)
		m.mu.Lock()
		for _, j := range m.jobs {
			j.cancel()
		}
		m.mu.Unlock()
		m.wg.Wait()
		m.mu.Lock()
		for _, j := range m.jobs {
			if j.file != "" {
				_ = os.Remove(j.file)
			}
		}
		m.jobs = map[string]*managedJob{}
		m.mu.Unlock()
	})
}
func (m *Manager) Create(ctx context.Context, root string, entries []Entry, authorizer Authorizer) (Job, error) {
	return m.create(ctx, 0, root, entries, authorizer)
}

// CreateForUser creates an archive private to userID. HTTP callers must use it.
func (m *Manager) CreateForUser(ctx context.Context, userID int64, root string, entries []Entry, authorizer Authorizer) (Job, error) {
	if userID <= 0 {
		return Job{}, errors.New("archive owner is required")
	}
	return m.create(ctx, userID, root, entries, authorizer)
}

func (m *Manager) create(ctx context.Context, userID int64, root string, entries []Entry, authorizer Authorizer) (Job, error) {
	if authorizer == nil {
		return Job{}, errors.New("archive authorizer is required")
	}
	if err := validate(root, entries, m.options); err != nil {
		return Job{}, err
	}
	for _, entry := range entries {
		ok, err := authorizer.Allow(ctx, entry.LogicalPath)
		if err != nil {
			return Job{}, err
		}
		if !ok {
			return Job{}, ErrForbidden
		}
	}
	id, err := newID()
	if err != nil {
		return Job{}, err
	}
	now := time.Now().UTC()
	jobctx, cancel := context.WithCancel(context.Background())
	j := &managedJob{Job: Job{ID: id, UserID: userID, Status: Running, CreatedAt: now, ExpiresAt: now.Add(m.options.TTL)}, cancel: cancel}
	m.mu.Lock()
	select {
	case <-m.closed:
		m.mu.Unlock()
		cancel()
		return Job{}, errors.New("archive manager closed")
	default:
		m.jobs[id] = j
	}
	job := j.Job
	m.mu.Unlock()
	select {
	case m.tasks <- task{id: id, entries: append([]Entry(nil), entries...), ctx: jobctx}:
		return job, nil
	case <-ctx.Done():
		m.Cancel(id)
		return Job{}, ctx.Err()
	case <-m.closed:
		m.Cancel(id)
		return Job{}, errors.New("archive manager closed")
	}
}

// OwnedBy is a boolean-only private-artifact access check.
func (m *Manager) OwnedBy(id string, userID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	return j != nil && j.UserID == userID && userID > 0
}

func (m *Manager) worker() {
	defer m.wg.Done()
	for {
		select {
		case t := <-m.tasks:
			m.build(t)
		case <-m.closed:
			return
		}
	}
}
func (m *Manager) build(t task) {
	f, err := os.CreateTemp(m.options.TempDir, "archive-*.zip")
	if err == nil {
		_ = f.Chmod(0600)
		err = m.writeZip(t.ctx, f, t.entries)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[t.id]
	if j == nil {
		if f != nil {
			_ = os.Remove(f.Name())
		}
		return
	}
	if t.ctx.Err() != nil {
		j.Status = Cancelled
		if f != nil {
			_ = os.Remove(f.Name())
		}
		return
	}
	if err != nil {
		j.Status = Failed
		j.Err = err
		if f != nil {
			_ = os.Remove(f.Name())
		}
		return
	}
	info, statErr := os.Stat(f.Name())
	if statErr != nil {
		j.Status = Failed
		j.Err = statErr
		return
	}
	j.file = f.Name()
	j.Size = info.Size()
	j.Status = Complete
}
func (m *Manager) writeZip(ctx context.Context, f *os.File, entries []Entry) error {
	z := zip.NewWriter(f)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			_ = z.Close()
			return err
		}
		name := strings.TrimPrefix(entry.LogicalPath, "/")
		w, err := z.Create(name)
		if err != nil {
			_ = z.Close()
			return err
		}
		r, err := m.source.Open(ctx, entry.ObjectKey)
		if err != nil {
			_ = z.Close()
			return err
		}
		_, copyErr := io.CopyBuffer(w, &contextReader{ctx: ctx, r: r}, make([]byte, 128*1024))
		closeErr := r.Close()
		if copyErr != nil {
			_ = z.Close()
			return copyErr
		}
		if closeErr != nil {
			_ = z.Close()
			return closeErr
		}
	}
	return z.Close()
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	if j == nil {
		return ErrNotFound
	}
	if j.Status == Running {
		j.cancel()
	}
	return nil
}
func (m *Manager) Job(id string) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	if j == nil {
		return Job{}, ErrNotFound
	}
	return j.Job, nil
}
func (m *Manager) Open(id string) (*os.File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	if j == nil || j.Status != Complete || j.file == "" {
		return nil, ErrNotFound
	}
	return os.Open(j.file)
}
func (m *Manager) Cleanup(now time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, j := range m.jobs {
		if !now.Before(j.ExpiresAt) && j.Status != Running {
			if j.file != "" {
				_ = os.Remove(j.file)
			}
			delete(m.jobs, id)
			n++
		}
	}
	return n
}
func validate(root string, entries []Entry, o Options) error {
	if !safeLogical(root) || len(entries) == 0 || len(entries) > o.MaxFiles {
		return errors.New("invalid archive selection")
	}
	var total int64
	for _, e := range entries {
		if !safeLogical(e.LogicalPath) || !within(root, e.LogicalPath) || e.ObjectKey == "" || e.Size < 0 {
			return errors.New("invalid archive entry")
		}
		if e.Size > o.MaxBytes-total {
			return errors.New("archive exceeds size limit")
		}
		total += e.Size
	}
	return nil
}
func safeLogical(v string) bool {
	if !strings.HasPrefix(v, "/") || strings.ContainsAny(v, "\\\x00") {
		return false
	}
	for _, s := range strings.Split(strings.TrimPrefix(v, "/"), "/") {
		if s == "" && v != "/" || s == "." || s == ".." {
			return false
		}
	}
	return path.Clean(v) == v
}
func within(root, p string) bool { return root == "/" || p == root || strings.HasPrefix(p, root+"/") }
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("archive id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
