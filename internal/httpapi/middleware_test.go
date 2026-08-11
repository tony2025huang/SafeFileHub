package httpapi

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/limits"
)

func TestLimitUploadReturns429AndRetryAfterWithoutCallingHandler(t *testing.T) {
	limiter, err := limits.NewUploadLimiter(1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := limiter.TryAcquire("alice", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	called := false
	h := LimitUpload(limiter, 7*time.Second, func(*http.Request) (string, string) { return "alice", "192.0.2.1" }, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/uploads", nil))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Retry-After"); got != "7" {
		t.Fatalf("Retry-After = %q, want 7", got)
	}
	if called {
		t.Fatal("limited request reached handler")
	}
}

func TestLimitUploadReleasesLeaseAfterSuccessErrorAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.Handler
		ctx     context.Context
	}{
		{"success", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }), context.Background()},
		{"error", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }), context.Background()},
		{"cancellation", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }), cancelledContext(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limiter, err := limits.NewUploadLimiter(1, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			h := LimitUpload(limiter, time.Second, func(*http.Request) (string, string) { return "alice", "192.0.2.1" }, tc.handler)
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/uploads", nil).WithContext(tc.ctx)
			if tc.name == "error" {
				func() { defer func() { _ = recover() }(); h.ServeHTTP(recorder, req) }()
			} else {
				h.ServeHTTP(recorder, req)
			}
			if lease, err := limiter.TryAcquire("alice", "192.0.2.1"); err != nil {
				t.Fatalf("lease not released: %v", err)
			} else {
				lease.Release()
			}
		})
	}
}

func TestRequestLimitsBodyAndIdleTimeout(t *testing.T) {
	cfg := config.Default()
	cfg.MaxRequestBodyBytes = 3
	cfg.RequestTimeout = 10 * time.Millisecond
	h := RequestLimits(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := r.Body.Read(make([]byte, 8))
		if err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		<-r.Context().Done()
		if !errors.Is(r.Context().Err(), context.DeadlineExceeded) {
			t.Errorf("context error = %v", r.Context().Err())
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/", &slowBody{data: []byte("a")}))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("idle status = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/", &slowBody{data: []byte("abcd")}))
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("body status = %d, want 413", recorder.Code)
	}
}

func cancelledContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

type slowBody struct{ data []byte }

func (b *slowBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		return 0, errors.New("EOF")
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}
func (b *slowBody) Close() error { return nil }

type blockingReadCloser struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{started: make(chan struct{}), closed: make(chan struct{})}
}

func (b *blockingReadCloser) Read([]byte) (int, error) {
	select {
	case <-b.started:
	default:
		close(b.started)
	}
	<-b.closed
	return 0, errors.New("body closed")
}
func (b *blockingReadCloser) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

func TestUploadBodyLimitsClosesBlockingBodyOnIdleTimeoutAndReleasesLease(t *testing.T) {
	limiter, err := limits.NewUploadLimiter(1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	body := newBlockingReadCloser()
	done := make(chan error, 1)
	h := UploadBodyLimits(20*time.Millisecond, 1024, LimitUpload(limiter, time.Second,
		func(*http.Request) (string, string) { return "alice", "192.0.2.1" },
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { _, err := r.Body.Read(make([]byte, 1)); done <- err })))
	req := httptest.NewRequest(http.MethodPost, "/uploads", nil)
	req.Body = body
	go h.ServeHTTP(httptest.NewRecorder(), req)
	select {
	case <-body.started:
	case <-time.After(time.Second):
		t.Fatal("read did not start")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocking read unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("blocking read was not interrupted")
	}
	if lease, err := limiter.TryAcquire("alice", "192.0.2.1"); err != nil {
		t.Fatalf("lease was not released: %v", err)
	} else {
		lease.Release()
	}
}

func TestUploadBodyLimitsInterruptsBlockingBodyOnRequestCancellation(t *testing.T) {
	body := newBlockingReadCloser()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	protected := protectUploadBody(ctx, body, time.Second)
	result := make(chan error, 1)
	go func() { _, err := protected.Read(make([]byte, 1)); result <- err }()
	select {
	case <-body.started:
	case <-time.After(time.Second):
		t.Fatal("read did not start")
	}
	cancel()
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt read")
	}
	protected.stop()
	select {
	case <-protected.done:
	case <-time.After(time.Second):
		t.Fatal("watcher did not exit")
	}
}

type pacedReadCloser struct {
	chunks [][]byte
	pause  time.Duration
}

func (b *pacedReadCloser) Read(p []byte) (int, error) {
	if len(b.chunks) == 0 {
		return 0, io.EOF
	}
	time.Sleep(b.pause)
	n := copy(p, b.chunks[0])
	b.chunks = b.chunks[1:]
	return n, nil
}
func (*pacedReadCloser) Close() error { return nil }

func TestUploadBodyLimitsRefreshesIdleDeadlineForStreamingBody(t *testing.T) {
	body := &pacedReadCloser{chunks: [][]byte{[]byte("a"), []byte("b"), []byte("c")}, pause: 10 * time.Millisecond}
	var got string
	h := UploadBodyLimits(25*time.Millisecond, 1024, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll: %v", err)
			return
		}
		got = string(data)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/uploads", body))
	if got != "abc" {
		t.Fatalf("stream = %q, want abc", got)
	}
}

type deadlineBody struct {
	blockingReadCloser
	deadlines atomic.Int32
}

func (b *deadlineBody) SetReadDeadline(deadline time.Time) error {
	b.deadlines.Add(1)
	if !deadline.After(time.Now()) { // model a connection deadline interrupting an in-flight Read
		_ = b.Close()
	}
	return nil
}

func TestUploadBodyLimitsUsesReadDeadlineWhenAvailableAndWatcherExits(t *testing.T) {
	body := &deadlineBody{blockingReadCloser: *newBlockingReadCloser()}
	protected := protectUploadBody(context.Background(), body, 20*time.Millisecond)
	result := make(chan error, 1)
	go func() { _, err := protected.Read(make([]byte, 1)); result <- err }()
	select {
	case <-body.started:
	case <-time.After(time.Second):
		t.Fatal("read did not start")
	}
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("read did not return")
	}
	if body.deadlines.Load() == 0 {
		t.Fatal("SetReadDeadline was not called")
	}
	protected.stop()
	select {
	case <-protected.done:
	case <-time.After(time.Second):
		t.Fatal("watcher did not exit")
	}
}

func TestUploadBodyLimitsInterruptsStalledHTTPUploadAndReleasesLease(t *testing.T) {
	limiter, err := limits.NewUploadLimiter(1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.UploadIdleTimeout = 25 * time.Millisecond
	h := RequestLimits(cfg, LimitUpload(limiter, time.Second,
		func(*http.Request) (string, string) { return "alice", "192.0.2.1" },
		UploadBodyLimits(cfg.UploadIdleTimeout, 0, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, err := r.Body.Read(make([]byte, 1))
			if err != nil {
				http.Error(w, "read upload", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}))))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := ServerTimeouts(cfg, h)
	defer server.Close()
	go server.Serve(listener)

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "POST /uploads HTTP/1.1\r\nHost: test\r\nContent-Length: 1\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("stalled upload did not return a response: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	if lease, err := limiter.TryAcquire("alice", "192.0.2.1"); err != nil {
		t.Fatalf("lease was not released after HTTP upload: %v", err)
	} else {
		lease.Release()
	}
}
