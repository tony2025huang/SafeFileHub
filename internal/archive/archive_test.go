package archive

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type testSource struct {
	files   map[string]string
	blocked chan struct{}
}

func (s testSource) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if s.blocked != nil {
		select {
		case <-s.blocked:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	v, ok := s.files[key]
	if !ok {
		return nil, errors.New("missing")
	}
	return io.NopCloser(strings.NewReader(v)), nil
}

type testAuth struct{ denied map[string]bool }

func (a testAuth) Allow(_ context.Context, path string) (bool, error) { return !a.denied[path], nil }

func TestArchiveRequiresPermissionForEveryEntryAndUsesLogicalNames(t *testing.T) {
	m, err := New(Options{Workers: 1, MaxFiles: 4, MaxBytes: 32, TTL: time.Hour, TempDir: t.TempDir()}, testSource{files: map[string]string{"a": "one", "b": "two"}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	_, err = m.Create(context.Background(), "/reports", []Entry{{LogicalPath: "/reports/a.txt", ObjectKey: "a", Size: 3}, {LogicalPath: "/reports/secret.txt", ObjectKey: "b", Size: 3}}, testAuth{denied: map[string]bool{"/reports/secret.txt": true}})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("Create error = %v, want forbidden", err)
	}
	job, err := m.Create(context.Background(), "/reports", []Entry{{LogicalPath: "/reports/a.txt", ObjectKey: "a", Size: 3}}, testAuth{})
	if err != nil {
		t.Fatal(err)
	}
	job = waitDone(t, m, job.ID)
	f, err := m.Open(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := zip.NewReader(f, job.Size)
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 1 || zr.File[0].Name != "reports/a.txt" {
		t.Fatalf("zip entries = %#v", zr.File)
	}
}
func TestArchiveRejectsLimitsAndTraversal(t *testing.T) {
	m, err := New(Options{Workers: 1, MaxFiles: 1, MaxBytes: 3, TTL: time.Hour, TempDir: t.TempDir()}, testSource{files: map[string]string{"a": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for _, entries := range [][]Entry{{{LogicalPath: "/a", ObjectKey: "a", Size: 2}, {LogicalPath: "/b", ObjectKey: "a", Size: 2}}, {{LogicalPath: "/a", ObjectKey: "a", Size: 4}}, {{LogicalPath: "/safe/../escape", ObjectKey: "a", Size: 1}}} {
		if _, err := m.Create(context.Background(), "/", entries, testAuth{}); err == nil {
			t.Fatalf("Create accepted %#v", entries)
		}
	}
}
func TestArchiveCancellationAndTTLCleanup(t *testing.T) {
	blocked := make(chan struct{})
	m, err := New(Options{Workers: 1, MaxFiles: 2, MaxBytes: 10, TTL: time.Millisecond, TempDir: t.TempDir()}, testSource{files: map[string]string{"a": "x"}, blocked: blocked})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	job, err := m.Create(context.Background(), "/", []Entry{{LogicalPath: "/a", ObjectKey: "a", Size: 1}}, testAuth{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Cancel(job.ID); err != nil {
		t.Fatal(err)
	}
	close(blocked)
	job = waitDone(t, m, job.ID)
	if job.Status != Cancelled {
		t.Fatalf("status = %q", job.Status)
	}
	job, err = m.Create(context.Background(), "/", []Entry{{LogicalPath: "/b", ObjectKey: "a", Size: 1}}, testAuth{})
	if err != nil {
		t.Fatal(err)
	}
	job = waitDone(t, m, job.ID)
	time.Sleep(2 * time.Millisecond)
	if got := m.Cleanup(time.Now()); got != 2 {
		t.Fatalf("cleanup = %d", got)
	}
	if _, err := m.Open(job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open after cleanup = %v", err)
	}
}
func waitDone(t *testing.T, m *Manager, id string) Job {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		j, err := m.Job(id)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status != Running {
			return j
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("job did not complete")
	return Job{}
}
