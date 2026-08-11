package limits

import (
	"errors"
	"sync"
)

// DownloadLimiter is a bounded, non-queuing global download admission gate.
type DownloadLimiter struct{ slots chan struct{} }

type DownloadLease struct {
	limiter *DownloadLimiter
	once    sync.Once
}

func NewDownloadLimiter(capacity int) (*DownloadLimiter, error) {
	if capacity <= 0 {
		return nil, errors.New("download limit must be positive")
	}
	return &DownloadLimiter{slots: make(chan struct{}, capacity)}, nil
}
func (l *DownloadLimiter) TryAcquire() (*DownloadLease, error) {
	select {
	case l.slots <- struct{}{}:
		return &DownloadLease{limiter: l}, nil
	default:
		return nil, ErrLimited
	}
}
func (l *DownloadLease) Release() {
	if l == nil || l.limiter == nil {
		return
	}
	l.once.Do(func() { <-l.limiter.slots })
}
