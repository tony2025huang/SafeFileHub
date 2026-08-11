// Package checksum computes upload MD5 values through the bounded repository task queue.
package checksum

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"
	"time"

	"github.com/example/safefilehub/internal/db"
)

type Tasks interface {
	RequeueComputingMD5Tasks(context.Context, int) (int, error)
	ClaimMD5Task(context.Context) (db.MD5Task, error)
	FileByID(context.Context, int64) (db.File, error)
	CompleteMD5Task(context.Context, int64, string) error
	FailMD5Task(context.Context, int64, string) error
}
type Objects interface {
	Open(string) (io.ReadCloser, error)
}
type Options struct{ Concurrency, MaxTasksPerRun, RecoveryLimit int }
type Worker struct {
	tasks     Tasks
	objects   Objects
	options   Options
	recovered bool
	mu        sync.Mutex
}

func NewWorker(tasks Tasks, objects Objects, options Options) *Worker {
	if options.Concurrency <= 0 {
		options.Concurrency = 1
	}
	if options.MaxTasksPerRun <= 0 {
		options.MaxTasksPerRun = 1
	}
	if options.RecoveryLimit <= 0 {
		options.RecoveryLimit = options.MaxTasksPerRun
	}
	return &Worker{tasks: tasks, objects: objects, options: options}
}

// Run continuously consumes bounded batches until ctx is cancelled. It performs crash
// recovery before the first batch, and never starts a second batch after cancellation.
// Callers own the goroutine used to invoke Run and should wait for it at shutdown.
func (w *Worker) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return errors.New("md5 worker interval must be positive")
	}
	for {
		if _, err := w.RunOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
}

// RunOnce first performs a single bounded crash recovery, then processes at most MaxTasksPerRun due tasks.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	w.mu.Lock()
	if !w.recovered {
		if _, err := w.tasks.RequeueComputingMD5Tasks(ctx, w.options.RecoveryLimit); err != nil {
			w.mu.Unlock()
			return 0, fmt.Errorf("recover md5 tasks: %w", err)
		}
		w.recovered = true
	}
	w.mu.Unlock()
	jobs := make(chan db.MD5Task)
	var wg sync.WaitGroup
	var done int
	var doneMu sync.Mutex
	for range w.options.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				w.process(ctx, t)
				doneMu.Lock()
				done++
				doneMu.Unlock()
			}
		}()
	}
	for range w.options.MaxTasksPerRun {
		if err := ctx.Err(); err != nil {
			close(jobs)
			wg.Wait()
			return done, err
		}
		t, err := w.tasks.ClaimMD5Task(ctx)
		if errors.Is(err, db.ErrNotFound) {
			break
		}
		if err != nil {
			close(jobs)
			wg.Wait()
			return done, fmt.Errorf("claim md5 task: %w", err)
		}
		select {
		case jobs <- t:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return done, ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	return done, nil
}
func (w *Worker) process(ctx context.Context, task db.MD5Task) {
	file, err := w.tasks.FileByID(ctx, task.FileID)
	if errors.Is(err, db.ErrNotFound) {
		return
	}
	if err != nil {
		_ = w.tasks.FailMD5Task(ctx, task.FileID, err.Error())
		return
	}
	r, err := w.objects.Open(file.ObjectKey)
	if errors.Is(err, fs.ErrNotExist) {
		_ = w.tasks.FailMD5Task(ctx, task.FileID, "object missing")
		return
	}
	if err != nil {
		_ = w.tasks.FailMD5Task(ctx, task.FileID, err.Error())
		return
	}
	defer r.Close()
	h := md5.New()
	if _, err = io.Copy(h, r); err != nil {
		_ = w.tasks.FailMD5Task(ctx, task.FileID, err.Error())
		return
	}
	_ = w.tasks.CompleteMD5Task(ctx, task.FileID, hex.EncodeToString(h.Sum(nil)))
}
