// Package publishedrecovery performs one bounded, cancellable recovery pass.
package publishedrecovery

import (
	"context"
	"errors"
	"fmt"
	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/storage"
	"log"
)

type repository interface {
	ObjectCleanupJobs(context.Context, int) ([]db.ObjectCleanupJob, error)
	CompleteObjectCleanup(context.Context, string) error
	DeletingFiles(context.Context, int) ([]db.File, error)
	FinalizeFileDelete(context.Context, int64, string) error
}
type Report struct{ CleanupChecked, CleanupCompleted, TombstonesChecked, TombstonesFinalized int }

func Recover(ctx context.Context, repo repository, store *storage.ObjectStore, limit int) (Report, error) {
	var out Report
	jobs, err := repo.ObjectCleanupJobs(ctx, limit)
	if err != nil {
		return out, fmt.Errorf("list object cleanup jobs: %w", err)
	}
	for _, j := range jobs {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		out.CleanupChecked++
		if err := store.Remove(j.ObjectKey); err != nil {
			log.Printf("published recovery cleanup object=%q failed: %v", j.ObjectKey, err)
			continue
		}
		if err := repo.CompleteObjectCleanup(ctx, j.ObjectKey); err != nil {
			return out, fmt.Errorf("complete cleanup: %w", err)
		}
		out.CleanupCompleted++
	}
	files, err := repo.DeletingFiles(ctx, limit)
	if err != nil {
		return out, fmt.Errorf("list deleting files: %w", err)
	}
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		out.TombstonesChecked++
		if err := store.Remove(f.ObjectKey); err != nil {
			log.Printf("published recovery tombstone id=%d object=%q failed: %v", f.ID, f.ObjectKey, err)
			continue
		}
		if err := repo.FinalizeFileDelete(ctx, f.ID, f.ObjectKey); err != nil && !errors.Is(err, db.ErrNotFound) {
			return out, fmt.Errorf("finalize tombstone: %w", err)
		}
		out.TombstonesFinalized++
	}
	return out, nil
}
