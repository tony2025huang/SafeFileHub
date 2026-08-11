package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/example/safefilehub/internal/config"
)

func TestHealthzReturnsSmallSuccessfulResponseWithoutStorageScan(t *testing.T) {
	cfg := config.Default()
	cfg.StorageRoot = "/path/that/must/not/be-scanned"

	h, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if got, want := recorder.Code, http.StatusOK; got != want {
		t.Fatalf("GET /healthz status = %d, want %d", got, want)
	}
	if body := recorder.Body.String(); body != "ok\n" {
		t.Fatalf("GET /healthz body = %q, want %q", body, "ok\n")
	}
	if size := recorder.Body.Len(); size > 64 {
		t.Fatalf("GET /healthz body is too large: %d bytes", size)
	}
}
