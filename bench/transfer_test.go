package bench

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestRunBoundedDoesNotCreatePendingGoroutines(t *testing.T) {
	started := make(chan struct{}, 128)
	release := make(chan struct{})
	var active, peak int
	var mu sync.Mutex

	transfer := func(context.Context, int) error {
		mu.Lock()
		active++
		if active > peak {
			peak = active
		}
		mu.Unlock()
		started <- struct{}{}
		<-release
		mu.Lock()
		active--
		mu.Unlock()
		return nil
	}

	before := runtime.NumGoroutine()
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), 128, 2, transfer) }()
	for range 2 {
		<-started
	}
	if got := runtime.NumGoroutine() - before; got > 12 {
		t.Fatalf("Run created %d goroutines with only two active transfers", got)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if peak != 2 {
		t.Fatalf("peak active transfers = %d, want 2", peak)
	}
}

func TestHealthzResponsiveDuringSixteenConcurrentUploads(t *testing.T) {
	uploadStarted := make(chan struct{}, 16)
	releaseUploads := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			_, _ = io.WriteString(w, "ok\n")
		case "/upload":
			uploadStarted <- struct{}{}
			<-releaseUploads
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{MaxConnsPerHost: 32}}
	var uploads sync.WaitGroup
	for range 16 {
		uploads.Add(1)
		go func() {
			defer uploads.Done()
			req, _ := http.NewRequest(http.MethodPost, server.URL+"/upload", nil)
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	for range 16 {
		<-uploadStarted
	}

	started := time.Now()
	resp, err := client.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", resp.StatusCode)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("health response took %s, budget is 250ms", elapsed)
	}
	close(releaseUploads)
	uploads.Wait()
}

func BenchmarkRun(b *testing.B) {
	payload := make([]byte, 1<<20)
	for _, concurrency := range []int{1, 2, 4, 8} {
		b.Run("concurrency="+strconv.Itoa(concurrency), func(b *testing.B) {
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := Run(context.Background(), concurrency, concurrency, func(context.Context, int) error {
					_, err := io.Copy(io.Discard, bytes.NewReader(payload))
					return err
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
