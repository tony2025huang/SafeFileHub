// Package metrics provides small, bounded in-process service counters.
package metrics

import (
	"fmt"
	"strings"
	"sync/atomic"
)

type Snapshot struct{ ActiveUploads, ActiveDownloads, ActiveArchives, ActiveLeases, TooManyRequests, Unavailable, Cancellations, Cleanup int64 }
type Metrics struct{ uploads, downloads, archives, leases, tooMany, unavailable, cancellations, cleanup atomic.Int64 }

func New() *Metrics                  { return &Metrics{} }
func (m *Metrics) UploadStarted()    { m.uploads.Add(1) }
func (m *Metrics) UploadFinished()   { m.uploads.Add(-1) }
func (m *Metrics) DownloadStarted()  { m.downloads.Add(1) }
func (m *Metrics) DownloadFinished() { m.downloads.Add(-1) }
func (m *Metrics) ArchiveStarted()   { m.archives.Add(1) }
func (m *Metrics) ArchiveFinished()  { m.archives.Add(-1) }
func (m *Metrics) LeaseStarted()     { m.leases.Add(1) }
func (m *Metrics) LeaseFinished()    { m.leases.Add(-1) }
func (m *Metrics) IncStatus(status int) {
	switch status {
	case 429:
		m.tooMany.Add(1)
	case 503:
		m.unavailable.Add(1)
	}
}
func (m *Metrics) IncCancellation() { m.cancellations.Add(1) }
func (m *Metrics) IncCleanup()      { m.cleanup.Add(1) }
func (m *Metrics) Snapshot() Snapshot {
	return Snapshot{m.uploads.Load(), m.downloads.Load(), m.archives.Load(), m.leases.Load(), m.tooMany.Load(), m.unavailable.Load(), m.cancellations.Load(), m.cleanup.Load()}
}

// Prometheus emits no request path, user, IP, or other unbounded labels.
func (m *Metrics) Prometheus() string {
	s := m.Snapshot()
	return fmt.Sprintf("# TYPE safefilehub_active_uploads gauge\nsafefilehub_active_uploads %d\n# TYPE safefilehub_active_downloads gauge\nsafefilehub_active_downloads %d\n# TYPE safefilehub_active_archives gauge\nsafefilehub_active_archives %d\n# TYPE safefilehub_active_leases gauge\nsafefilehub_active_leases %d\n# TYPE safefilehub_http_responses_total counter\nsafefilehub_http_responses_total{status=\"429\"} %d\nsafefilehub_http_responses_total{status=\"503\"} %d\n# TYPE safefilehub_cancellations_total counter\nsafefilehub_cancellations_total %d\n# TYPE safefilehub_cleanup_total counter\nsafefilehub_cleanup_total %d\n", s.ActiveUploads, s.ActiveDownloads, s.ActiveArchives, s.ActiveLeases, s.TooManyRequests, s.Unavailable, s.Cancellations, s.Cleanup)
}
func (m *Metrics) String() string { return strings.TrimSpace(m.Prometheus()) }
