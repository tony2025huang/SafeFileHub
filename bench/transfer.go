// Package bench supplies a local, bounded transfer benchmark harness.
package bench

import (
	"context"
	"errors"
	"sync"
)

// Transfer performs one transfer identified by index. It must honor ctx.
type Transfer func(ctx context.Context, index int) error

// Run executes count transfers with at most concurrency active at once. It
// schedules work from the caller goroutine and deliberately has no pending
// goroutine-per-transfer queue.
func Run(ctx context.Context, count, concurrency int, transfer Transfer) error {
	if count < 0 {
		return errors.New("transfer count must not be negative")
	}
	if concurrency <= 0 {
		return errors.New("transfer concurrency must be positive")
	}
	if transfer == nil {
		return errors.New("transfer function is required")
	}
	if count == 0 {
		return nil
	}
	if concurrency > count {
		concurrency = count
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	errs := make(chan error, 1)
	var workers sync.WaitGroup
	for range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				if err := transfer(ctx, index); err != nil {
					select {
					case errs <- err:
						cancel()
					default:
					}
					return
				}
			}
		}()
	}

	for index := range count {
		select {
		case <-ctx.Done():
			break
		case jobs <- index:
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	workers.Wait()
	select {
	case err := <-errs:
		return err
	default:
		return ctx.Err()
	}
}
