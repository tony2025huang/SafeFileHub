package siteassets

import (
	"context"
	"errors"
	"testing"
)

type cleanupTasks struct {
	keys      []string
	completed []string
}

func (t *cleanupTasks) SiteAssetCleanupKeys(context.Context, int) ([]string, error) {
	return append([]string(nil), t.keys...), nil
}
func (t *cleanupTasks) CompleteSiteAssetCleanup(_ context.Context, key string) error {
	t.completed = append(t.completed, key)
	return nil
}

type cleanupStore struct {
	failures map[string]error
	removed  []string
}

func (s *cleanupStore) Remove(key string) error {
	if err := s.failures[key]; err != nil {
		return err
	}
	s.removed = append(s.removed, key)
	return nil
}

func TestRecoverCleanupIsBoundedAndRetrySafe(t *testing.T) {
	tasks := &cleanupTasks{keys: []string{"old", "retry"}}
	store := &cleanupStore{failures: map[string]error{"retry": errors.New("disk offline")}}
	n, err := RecoverCleanup(context.Background(), tasks, store, 1)
	if err != nil || n != 1 || len(tasks.completed) != 1 || tasks.completed[0] != "old" {
		t.Fatalf("first recovery n=%d completed=%v err=%v", n, tasks.completed, err)
	}
	tasks.keys = []string{"retry"}
	delete(store.failures, "retry")
	n, err = RecoverCleanup(context.Background(), tasks, store, 16)
	if err != nil || n != 1 || len(tasks.completed) != 2 || tasks.completed[1] != "retry" {
		t.Fatalf("retry recovery n=%d completed=%v err=%v", n, tasks.completed, err)
	}
}
