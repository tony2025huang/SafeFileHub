package httpapi

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/limits"
	"github.com/example/safefilehub/internal/metrics"
)

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}
func metricResponses(m *metrics.Metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rw, r)
		if rw.status == 0 {
			rw.status = http.StatusOK
		}
		m.IncStatus(rw.status)
	})
}

type UploadIdentity func(*http.Request) (user, ip string)

func LimitUpload(limiter *limits.UploadLimiter, retryAfter time.Duration, identity UploadIdentity, next http.Handler) http.Handler {
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ip := identity(r)
		lease, err := limiter.TryAcquire(user, ip)
		if err != nil {
			w.Header().Set("Retry-After", strconv.FormatInt(int64(retryAfter.Round(time.Second)/time.Second), 10))
			http.Error(w, "upload capacity exceeded; retry later", http.StatusTooManyRequests)
			return
		}
		defer lease.Release()
		next.ServeHTTP(w, r)
	})
}

// RequestLimits applies body limits and only an explicitly configured total
// handler deadline. Socket timeouts are independently configured below.
func RequestLimits(cfg config.Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cfg.RequestTimeout > 0 {
			ctx, cancel := context.WithTimeout(r.Context(), cfg.RequestTimeout)
			defer cancel()
			r = r.WithContext(ctx)
		}
		limited := http.MaxBytesReader(w, r.Body, cfg.MaxRequestBodyBytes)
		if setter, ok := r.Body.(readDeadlineSetter); ok {
			r.Body = maxBytesBody{ReadCloser: limited, deadlines: setter}
		} else {
			r.Body = limited
		}
		next.ServeHTTP(w, r)
	})
}

func requestTooLarge(err error) bool { var maxErr *http.MaxBytesError; return errors.As(err, &maxErr) }

type connectionKey struct{}

func ServerTimeouts(cfg config.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr: cfg.ListenAddr, Handler: handler, ReadHeaderTimeout: cfg.ReadHeaderTimeout, ReadTimeout: cfg.ReadTimeout, WriteTimeout: cfg.WriteTimeout, IdleTimeout: cfg.IdleTimeout,
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			return context.WithValue(ctx, connectionKey{}, conn)
		},
	}
}

// readDeadlineSetter is implemented by HTTP server request bodies backed by a
// connection. Keeping this capability through size limiting lets upload reads
// use the connection's native idle deadline when it is available.
type readDeadlineSetter interface {
	SetReadDeadline(time.Time) error
}

type maxBytesBody struct {
	io.ReadCloser
	deadlines readDeadlineSetter
}

func (b maxBytesBody) SetReadDeadline(deadline time.Time) error {
	if b.deadlines == nil {
		return nil
	}
	return b.deadlines.SetReadDeadline(deadline)
}

// UploadBodyLimits protects upload request bodies from a stalled peer. It is
// deliberately upload-specific: unlike RequestTimeout, UploadIdleTimeout is
// refreshed for every successful read so a continuously streaming upload is
// not subject to a total wall-clock deadline. On timeout or cancellation it
// closes ordinary bodies to interrupt a blocked Read; connection-backed bodies
// additionally receive a read deadline.
func UploadBodyLimits(idle time.Duration, maxBytes int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := r.Body
		if maxBytes > 0 {
			body = http.MaxBytesReader(w, body, maxBytes)
		}
		protected := protectUploadBody(r.Context(), body, idle)
		defer protected.stop()
		r = r.Clone(r.Context())
		r.Body = protected
		next.ServeHTTP(w, r)
	})
}

type uploadBody struct {
	body     io.ReadCloser
	idle     time.Duration
	deadline readDeadlineSetter
	stopOnce sync.Once
	stopCh   chan struct{}
	done     chan struct{}
	resetMu  sync.Mutex
	timer    *time.Timer
}

func protectUploadBody(ctx context.Context, body io.ReadCloser, idle time.Duration) *uploadBody {
	p := &uploadBody{body: body, idle: idle}
	if setter, ok := body.(readDeadlineSetter); ok {
		p.deadline = setter
	} else if conn, ok := ctx.Value(connectionKey{}).(net.Conn); ok {
		p.deadline = conn
	}
	if idle <= 0 {
		return p
	}
	p.stopCh = make(chan struct{})
	p.done = make(chan struct{})
	p.timer = time.NewTimer(idle)
	p.setDeadline(time.Now().Add(idle))
	go p.watch(ctx)
	return p
}

func (p *uploadBody) Read(b []byte) (int, error) {
	n, err := p.body.Read(b)
	if n > 0 && p.idle > 0 {
		p.refresh()
	}
	return n, err
}

func (p *uploadBody) Close() error {
	p.stop()
	return p.body.Close()
}

func (p *uploadBody) watch(ctx context.Context) {
	defer close(p.done)
	select {
	case <-p.stopCh:
	case <-ctx.Done():
		p.interrupt()
	case <-p.timer.C:
		p.interrupt()
	}
}

func (p *uploadBody) interrupt() {
	if p.deadline != nil {
		p.setDeadline(time.Now())
		return
	}
	_ = p.body.Close()
}

func (p *uploadBody) refresh() {
	p.resetMu.Lock()
	defer p.resetMu.Unlock()
	if !p.timer.Stop() {
		select {
		case <-p.timer.C:
		default:
		}
	}
	p.timer.Reset(p.idle)
	p.setDeadline(time.Now().Add(p.idle))
}

func (p *uploadBody) setDeadline(deadline time.Time) {
	if p.deadline != nil {
		_ = p.deadline.SetReadDeadline(deadline)
	}
}

func (p *uploadBody) stop() {
	if p.idle <= 0 {
		return
	}
	p.stopOnce.Do(func() {
		close(p.stopCh)
		p.resetMu.Lock()
		if !p.timer.Stop() {
			select {
			case <-p.timer.C:
			default:
			}
		}
		p.resetMu.Unlock()
		<-p.done
		p.setDeadline(time.Time{})
	})
}
