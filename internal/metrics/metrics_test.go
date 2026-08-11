package metrics

import (
	"strings"
	"testing"
)

func TestMetricsUseAtomicBoundedCounters(t *testing.T) {
	m := New()
	m.UploadStarted()
	m.UploadStarted()
	m.UploadFinished()
	m.DownloadStarted()
	m.LeaseStarted()
	m.LeaseFinished()
	m.IncStatus(429)
	m.IncStatus(503)
	m.IncStatus(500)
	m.IncCancellation()
	m.IncCleanup()
	s := m.Snapshot()
	if s.ActiveUploads != 1 || s.ActiveDownloads != 1 || s.ActiveArchives != 0 || s.ActiveLeases != 0 || s.TooManyRequests != 1 || s.Unavailable != 1 || s.Cancellations != 1 || s.Cleanup != 1 {
		t.Fatalf("unexpected snapshot: %#v", s)
	}
	text := m.Prometheus()
	if strings.Contains(text, "500") || strings.Contains(text, "path") || !strings.Contains(text, "safefilehub_http_responses_total{status=\"429\"} 1") {
		t.Fatalf("metrics must be bounded and path-free: %s", text)
	}
}
