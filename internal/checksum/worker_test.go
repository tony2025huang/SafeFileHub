package checksum

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/example/safefilehub/internal/db"
)

type fakeTasks struct {
	tasks     []db.MD5Task
	files     map[int64]db.File
	complete  map[int64]string
	failed    map[int64]string
	recovered int
	mu        sync.Mutex
}

func (f *fakeTasks) RequeueComputingMD5Tasks(context.Context, int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recovered++
	return 0, nil
}
func (f *fakeTasks) recoveryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recovered
}
func (f *fakeTasks) ClaimMD5Task(context.Context) (db.MD5Task, error) {
	if len(f.tasks) == 0 {
		return db.MD5Task{}, db.ErrNotFound
	}
	t := f.tasks[0]
	f.tasks = f.tasks[1:]
	return t, nil
}
func (f *fakeTasks) FileByID(_ context.Context, id int64) (db.File, error) {
	v, ok := f.files[id]
	if !ok {
		return db.File{}, db.ErrNotFound
	}
	return v, nil
}
func (f *fakeTasks) CompleteMD5Task(_ context.Context, id int64, d string) error {
	f.complete[id] = d
	return nil
}
func (f *fakeTasks) FailMD5Task(_ context.Context, id int64, e string) error {
	f.failed[id] = e
	return nil
}

type localObjects struct{ root string }

func (s localObjects) Open(key string) (io.ReadCloser, error) {
	return os.Open(filepath.Join(s.root, key))
}

func TestWorkerRecoversAndStreamsClaimedTasks(t *testing.T) {
	dir := t.TempDir()
	key := "large.bin"
	data := make([]byte, 2<<20+13)
	for i := range data {
		data[i] = byte(i)
	}
	if err := os.WriteFile(filepath.Join(dir, key), data, 0600); err != nil {
		t.Fatal(err)
	}
	want := md5.Sum(data)
	f := &fakeTasks{tasks: []db.MD5Task{{FileID: 7}}, files: map[int64]db.File{7: {ID: 7, ObjectKey: key}}, complete: map[int64]string{}, failed: map[int64]string{}}
	w := NewWorker(f, localObjects{dir}, Options{Concurrency: 2, MaxTasksPerRun: 4, RecoveryLimit: 3})
	if n, err := w.RunOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("RunOnce=(%d,%v), want (1,nil)", n, err)
	}
	if f.recoveryCount() != 1 {
		t.Fatalf("recoveries=%d", f.recovered)
	}
	if got := f.complete[7]; got != hex.EncodeToString(want[:]) {
		t.Fatalf("digest=%q", got)
	}
}
func TestWorkerRunStopsOnCancellation(t *testing.T) {
	f := &fakeTasks{files: map[int64]db.File{}, complete: map[int64]string{}, failed: map[int64]string{}}
	w := NewWorker(f, localObjects{t.TempDir()}, Options{MaxTasksPerRun: 1})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, time.Hour) }()
	deadline := time.After(time.Second)
	for f.recoveryCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("worker did not start")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
}

func TestWorkerTreatsDeletedFileAsIdempotent(t *testing.T) {
	f := &fakeTasks{tasks: []db.MD5Task{{FileID: 7}}, files: map[int64]db.File{}, complete: map[int64]string{}, failed: map[int64]string{}}
	w := NewWorker(f, localObjects{t.TempDir()}, Options{MaxTasksPerRun: 1})
	if n, err := w.RunOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("RunOnce=(%d,%v)", n, err)
	}
	if len(f.failed) != 0 || len(f.complete) != 0 {
		t.Fatalf("deleted file was completed/failed: %#v %#v", f.complete, f.failed)
	}
}
