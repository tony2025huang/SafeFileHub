package observability

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// RotatingWriter writes to path and rotates before a write would exceed maxBytes.
// Rotated files are named path.<UTC timestamp>; retention removes only those files.
type RotatingWriter struct {
	path          string
	maxBytes      int64
	retentionDays int
	backups       int
	mu            sync.Mutex
	file          *os.File
	size          int64
}

func NewRotatingWriter(path string, maxBytes int64, retentionDays, backups int) (*RotatingWriter, error) {
	if path == "" {
		return nil, fmt.Errorf("log path is empty")
	}
	if maxBytes < 0 || retentionDays < 0 || backups < 0 {
		return nil, fmt.Errorf("invalid log rotation settings")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	w := &RotatingWriter{path: path, maxBytes: maxBytes, retentionDays: retentionDays, backups: backups}
	if err := w.open(); err != nil {
		return nil, err
	}
	if err := w.cleanup(time.Now().UTC()); err != nil {
		_ = w.file.Close()
		return nil, err
	}
	return w, nil
}
func (w *RotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file, w.size = f, info.Size()
	return nil
}
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.maxBytes > 0 && w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}
func (w *RotatingWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	rotated := w.path + "." + time.Now().UTC().Format("20060102T150405.000000000Z")
	if err := os.Rename(w.path, rotated); err != nil {
		return fmt.Errorf("rotate log: %w", err)
	}
	if err := w.open(); err != nil {
		return err
	}
	return w.cleanup(time.Now().UTC())
}
func (w *RotatingWriter) cleanup(now time.Time) error {
	matches, err := filepath.Glob(w.path + ".*")
	if err != nil {
		return err
	}
	type entry struct {
		path string
		mod  time.Time
	}
	entries := make([]entry, 0, len(matches))
	for _, p := range matches {
		info, e := os.Stat(p)
		if e == nil && !info.IsDir() {
			entries = append(entries, entry{p, info.ModTime()})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].mod.After(entries[j].mod) })
	for i, e := range entries {
		if (w.retentionDays > 0 && e.mod.Before(now.AddDate(0, 0, -w.retentionDays))) || (w.backups > 0 && i >= w.backups) {
			if err := os.Remove(e.path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

var _ io.WriteCloser = (*RotatingWriter)(nil)
