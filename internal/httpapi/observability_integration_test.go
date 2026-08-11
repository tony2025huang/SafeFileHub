package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/limits"
	"github.com/example/safefilehub/internal/metrics"
)

func TestObservedHandlerSharesMetricsAcrossTransferAdmissionAndResponses(t *testing.T) {
	m := metrics.New()
	limiter, err := limits.NewUploadLimiter(1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	occupied, err := limiter.TryAcquire("alice", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Release()

	mux := http.NewServeMux()
	mux.Handle("POST /uploads", limitUploadWithMetrics(limiter, time.Second, func(*http.Request) (string, string) { return "alice", "192.0.2.1" }, m, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	mux.HandleFunc("GET /unavailable", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusServiceUnavailable) })
	h := observedHandler(config.Default(), mux, m)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/uploads", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/unavailable", nil))
	s := m.Snapshot()
	if s.TooManyRequests != 1 || s.Unavailable != 1 {
		t.Fatalf("status counters = %#v, want one 429 and one 503", s)
	}
	if s.ActiveLeases != 0 || s.ActiveUploads != 0 {
		t.Fatalf("admission gauges leaked: %#v", s)
	}
}

func TestNewServerWithUploadsAndObservabilityRejectsNilMetrics(t *testing.T) {
	_, err := NewServerWithUploadsAndObservability(config.Default(), rejectingAuthenticator{}, nil, nil, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected observability dependency error")
	}
}
