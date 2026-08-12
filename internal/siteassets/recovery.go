package siteassets

import "context"

// CleanupTasks is the durable, retry-safe cleanup queue for superseded assets.
type CleanupTasks interface {
	SiteAssetCleanupKeys(context.Context, int) ([]string, error)
	CompleteSiteAssetCleanup(context.Context, string) error
}

type remover interface{ Remove(string) error }

// RecoverCleanup deletes at most limit queued objects. A queue row is removed
// only after its object deletion succeeds (or is idempotently already absent),
// so failed deletions remain durable for a later startup or request pass.
func RecoverCleanup(ctx context.Context, tasks CleanupTasks, store remover, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	keys, err := tasks.SiteAssetCleanupKeys(ctx, limit)
	if err != nil {
		return 0, err
	}
	completed := 0
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return completed, err
		}
		if err := store.Remove(key); err != nil {
			continue
		}
		if err := tasks.CompleteSiteAssetCleanup(ctx, key); err != nil {
			return completed, err
		}
		completed++
	}
	return completed, nil
}
