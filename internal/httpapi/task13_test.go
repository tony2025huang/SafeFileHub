package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/metrics"
)

type task13Checks struct{ database, storage, disk error }

func (c task13Checks) Database(r *http.Request) error { return c.database }
func (c task13Checks) Storage(r *http.Request) error  { return c.storage }
func (c task13Checks) Disk(r *http.Request) error     { return c.disk }

func TestHealthDoesNotRunReadinessChecks(t *testing.T) {
	h, err := NewServerWithObservability(config.Default(), task13Checks{database: errors.New("down")}, metrics.New())
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 || rec.Body.String() != "ok\n" {
		t.Fatalf("health = %d %q", rec.Code, rec.Body.String())
	}
}
func TestReadyReportsDependencyFailures(t *testing.T) {
	for _, c := range []task13Checks{{database: errors.New("down")}, {storage: errors.New("gone")}, {disk: errors.New("full")}} {
		h, err := NewServerWithObservability(config.Default(), c, metrics.New())
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("ready = %d", rec.Code)
		}
	}
	h, _ := NewServerWithObservability(config.Default(), task13Checks{}, metrics.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != 200 {
		t.Fatalf("ready = %d", rec.Code)
	}
}
func TestMetricsEndpointAndStatusCounting(t *testing.T) {
	m := metrics.New()
	h, err := NewServerWithObservability(config.Default(), task13Checks{}, m)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/missing", nil))
	if rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "safefilehub_http_responses_total") {
		t.Fatalf("metrics = %d %s", rec.Code, rec.Body.String())
	}
}
