// Package limits provides non-blocking, bounded concurrency controls.
package limits

import (
	"errors"
	"sync"
)

// ErrLimited means capacity is currently unavailable. Callers should retry
// later rather than wait in the service.
var ErrLimited = errors.New("upload capacity exceeded")

// UploadKey identifies the two scoped upload limits.
type UploadKey struct {
	User string
	IP   string
}

// UploadLimiter caps concurrent uploads globally and for each user and IP.
// It never queues callers: TryAcquire either returns a lease immediately or
// ErrLimited.
type UploadLimiter struct {
	global  chan struct{}
	perUser int
	perIP   int

	mu    sync.Mutex
	users map[string]int
	ips   map[string]int
}

func NewUploadLimiter(global, perUser, perIP int) (*UploadLimiter, error) {
	if global <= 0 || perUser <= 0 || perIP <= 0 {
		return nil, errors.New("upload limits must be positive")
	}
	return &UploadLimiter{global: make(chan struct{}, global), perUser: perUser, perIP: perIP, users: make(map[string]int), ips: make(map[string]int)}, nil
}

// TryAcquire obtains all three capacity slots without blocking.
func (l *UploadLimiter) TryAcquire(user, ip string) (*Lease, error) {
	select {
	case l.global <- struct{}{}:
	default:
		return nil, ErrLimited
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.users[user] >= l.perUser || l.ips[ip] >= l.perIP {
		<-l.global
		return nil, ErrLimited
	}
	l.users[user]++
	l.ips[ip]++
	return &Lease{limiter: l, key: UploadKey{User: user, IP: ip}}, nil
}

// Lease represents acquired capacity. Release is safe to call more than once.
type Lease struct {
	limiter *UploadLimiter
	key     UploadKey
	once    sync.Once
}

func (l *Lease) Release() {
	if l == nil || l.limiter == nil {
		return
	}
	l.once.Do(func() {
		l.limiter.mu.Lock()
		l.limiter.users[l.key.User]--
		if l.limiter.users[l.key.User] == 0 {
			delete(l.limiter.users, l.key.User)
		}
		l.limiter.ips[l.key.IP]--
		if l.limiter.ips[l.key.IP] == 0 {
			delete(l.limiter.ips, l.key.IP)
		}
		l.limiter.mu.Unlock()
		<-l.limiter.global
	})
}
