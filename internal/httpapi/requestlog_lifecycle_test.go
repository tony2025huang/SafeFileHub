package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/example/safefilehub/internal/archive"
	"github.com/example/safefilehub/internal/auth"
	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/limits"
	"github.com/example/safefilehub/internal/metrics"
	appLog "github.com/example/safefilehub/internal/observability"
	"github.com/example/safefilehub/internal/permission"
	"github.com/example/safefilehub/internal/storage"
)

func TestApplicationLogTransferLifecycleAndTerminalCoverage(t *testing.T) {
	var out bytes.Buffer
	logger := appLog.NewMulti(appLog.New(&out, appLog.FormatJSON))
	mux := http.NewServeMux()
	mux.Handle("POST /login", http.HandlerFunc(ok))
	mux.Handle("POST /logout", http.HandlerFunc(ok))
	mux.Handle("GET /roots/{rootID}/files", http.HandlerFunc(ok))
	mux.Handle("POST /api/admin/users", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "forbidden", http.StatusForbidden) }))
	mux.Handle("POST /api/uploads", logTransferLifecycle("upload", http.HandlerFunc(ok)))
	mux.Handle("GET /api/files/{fileID}", logTransferLifecycle("download", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("data")) })))
	mux.Handle("POST /api/roots/{rootID}/archives", logTransferLifecycle("archive", http.HandlerFunc(ok)))
	h := requestContext(config.Default(), applicationLogWithLogger(logger, mux))

	for _, tc := range []struct{ method, path string }{
		{"POST", "/login"}, {"POST", "/logout"}, {"GET", "/roots/1/files"}, {"POST", "/api/admin/users"},
		{"POST", "/api/uploads"}, {"GET", "/api/files/file-A"}, {"POST", "/api/roots/1/archives"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil).WithContext(context.WithValue(context.Background(), sessionAuditIDKey{}, "audit-safe"))
		r.RemoteAddr = "192.0.2.7:1234"
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	var events []appLog.Event
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var e appLog.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	want := map[string]bool{"login": false, "logout": false, "file.list": false, "admin": false, "upload.start": false, "upload.complete": false, "download.start": false, "download.complete": false, "archive.start": false, "archive.complete": false}
	requestIDs := map[string]map[string]bool{}
	transferIDs := map[string]bool{}
	for _, e := range events {
		if _, ok := want[e.Operation]; ok {
			want[e.Operation] = true
		}
		if e.RequestID == "" {
			t.Fatal("missing request_id")
		}
		if requestIDs[e.RequestID] == nil {
			requestIDs[e.RequestID] = map[string]bool{}
		}
		requestIDs[e.RequestID][e.Operation] = true
		if e.TransferID != "" {
			transferIDs[e.TransferID] = true
		}
		if strings.Contains(strings.ToLower(e.ErrorCode), "password") {
			t.Fatal("sensitive error code")
		}
	}
	for op, seen := range want {
		if !seen {
			t.Errorf("missing %s in %#v", op, events)
		}
	}
	if len(requestIDs) != 7 {
		t.Fatalf("request IDs=%v, want seven independent requests", requestIDs)
	}
	// The synthetic archive-create handler does not allocate a job. A real
	// create path is covered below and only emits archive-<jobID> after success.
	if len(transferIDs) != 1 || !transferIDs["file-file-A"] {
		t.Fatalf("transfer IDs=%v, want only the download ID", transferIDs)
	}
}
func ok(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

func TestOperationForShortAndUnknownUploadPathsDoesNotPanic(t *testing.T) {
	for _, path := range []string{"/", "/api/u", "/api/uploads/x"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		_ = operationFor(r)
	}
}

type productionLogFixture struct {
	handler http.Handler
	repo    *db.Repository
	root    db.StorageRoot
}

func TestProductionRequestLogRealTransferLifecycles(t *testing.T) {
	var logs bytes.Buffer
	fx := newProductionLogFixture(t)
	ts := httptest.NewServer(WithApplicationLogger(appLog.NewMulti(appLog.New(&logs, appLog.FormatJSON)), fx.handler))
	defer ts.Close()
	client := ts.Client()
	login := mustHTTP(t, client, http.MethodPost, ts.URL+"/login", `{"username":"alice","password":"correct password"}`, nil)
	if login.StatusCode != http.StatusNoContent {
		t.Fatalf("login=%d", login.StatusCode)
	}
	cookie := login.Cookies()[0]
	_ = login.Body.Close()
	create := mustHTTP(t, client, http.MethodPost, ts.URL+"/api/uploads", `{"root_id":1,"path":"reports/upload.txt","size":5}`, cookie)
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("create=%d: %s", create.StatusCode, readBody(t, create))
	}
	var up uploadResponse
	if err := json.NewDecoder(create.Body).Decode(&up); err != nil {
		t.Fatal(err)
	}
	_ = create.Body.Close()
	r, err := http.NewRequest(http.MethodPatch, ts.URL+"/api/uploads/"+up.UploadID, strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/offset+octet-stream")
	r.Header.Set("Upload-Offset", "0")
	r.AddCookie(cookie)
	patch, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	if patch.StatusCode != http.StatusNoContent {
		t.Fatalf("patch=%d: %s", patch.StatusCode, readBody(t, patch))
	}
	_ = patch.Body.Close()
	complete := mustHTTP(t, client, http.MethodPost, ts.URL+"/api/uploads/"+up.UploadID+"/complete", "", cookie)
	if complete.StatusCode != http.StatusNoContent {
		t.Fatalf("complete=%d: %s", complete.StatusCode, readBody(t, complete))
	}
	_ = complete.Body.Close()
	file, err := fx.repo.FileByRootAndPath(context.Background(), fx.root.ID, "/reports/upload.txt")
	if err != nil {
		t.Fatal(err)
	}
	download := mustHTTP(t, client, http.MethodGet, ts.URL+"/api/files/"+strconv.FormatInt(file.ID, 10), "", cookie)
	if download.StatusCode != http.StatusOK || readBody(t, download) != "hello" {
		t.Fatalf("download=%d", download.StatusCode)
	}
	missing := mustHTTP(t, client, http.MethodGet, ts.URL+"/api/files/99999", "", cookie)
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing download=%d", missing.StatusCode)
	}
	_ = missing.Body.Close()
	createArchive := mustHTTP(t, client, http.MethodPost, ts.URL+"/api/roots/1/archives", `{"path":"/reports"}`, cookie)
	if createArchive.StatusCode != http.StatusAccepted {
		t.Fatalf("archive create=%d: %s", createArchive.StatusCode, readBody(t, createArchive))
	}
	var job struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(createArchive.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	_ = createArchive.Body.Close()
	var archiveDownload *http.Response
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		archiveDownload = mustHTTP(t, client, http.MethodGet, ts.URL+"/api/archives/"+job.ID, "", cookie)
		if archiveDownload.StatusCode == http.StatusOK {
			break
		}
		_ = archiveDownload.Body.Close()
		time.Sleep(5 * time.Millisecond)
	}
	if archiveDownload == nil || archiveDownload.StatusCode != http.StatusOK {
		t.Fatal("archive download did not complete")
	}
	if len(readBody(t, archiveDownload)) == 0 {
		t.Fatal("empty archive")
	}
	events := decodeLogEvents(t, logs.String())
	assertLifecycle(t, events, "upload", up.UploadID)
	assertLifecycle(t, events, "download", "file-"+strconv.FormatInt(file.ID, 10))
	assertLifecycle(t, events, "archive", "archive-"+job.ID)
	for _, e := range events {
		if (e.Operation == "archive.start" || e.Operation == "archive.complete") && e.Route == "/api/roots/1/archives" && e.TransferID != "archive-"+job.ID {
			t.Fatalf("archive create lifecycle transfer_id = %q, want archive-%s", e.TransferID, job.ID)
		}
	}
	for _, e := range events {
		if (e.Operation == "upload.complete" || e.Operation == "download.complete" || e.Operation == "archive.complete") && e.SessionAuditID == "" {
			t.Fatalf("%s lacks session correlation", e.Operation)
		}
	}
}
func TestProductionArchiveCreateFailureLogsRequestWithoutUncorrelatedLifecycle(t *testing.T) {
	var logs bytes.Buffer
	fx := newProductionLogFixture(t)
	ts := httptest.NewServer(WithApplicationLogger(appLog.NewMulti(appLog.New(&logs, appLog.FormatJSON)), fx.handler))
	defer ts.Close()

	login := mustHTTP(t, ts.Client(), http.MethodPost, ts.URL+"/login", `{"username":"alice","password":"correct password"}`, nil)
	if login.StatusCode != http.StatusNoContent {
		t.Fatalf("login=%d", login.StatusCode)
	}
	cookie := login.Cookies()[0]
	_ = login.Body.Close()

	// Invalid JSON fails before archive.Manager can create a job, so there is no
	// stable transfer ID to emit. The enclosing request event remains auditable.
	failed := mustHTTP(t, ts.Client(), http.MethodPost, ts.URL+"/api/roots/1/archives", `{`, cookie)
	if failed.StatusCode != http.StatusBadRequest {
		t.Fatalf("archive create failure=%d: %s", failed.StatusCode, readBody(t, failed))
	}
	_ = failed.Body.Close()

	events := decodeLogEvents(t, logs.String())
	var requestEvent *appLog.Event
	for i := range events {
		e := &events[i]
		if e.Operation == "archive.request" && e.Route == "/api/roots/1/archives" {
			requestEvent = e
		}
		if (e.Operation == "archive.start" || e.Operation == "archive.complete") && e.Route == "/api/roots/1/archives" {
			t.Fatalf("failed archive creation emitted uncorrelated lifecycle event: %#v", e)
		}
	}
	if requestEvent == nil || requestEvent.Success || requestEvent.Status != http.StatusBadRequest || requestEvent.RequestID == "" || requestEvent.SessionAuditID == "" {
		t.Fatalf("failed archive request event = %#v", requestEvent)
	}
}

func newProductionLogFixture(t *testing.T) productionLogFixture {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StorageRoot = filepath.Join(dir, "objects")
	cfg.SQLitePath = filepath.Join(dir, "db")
	archiveDir := filepath.Join(dir, "archives")
	if err := os.MkdirAll(cfg.StorageRoot, 0700); err != nil {
		t.Fatal(err)
	}
	repo, err := db.Open(context.Background(), cfg.SQLitePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	hash, err := auth.HashPassword("correct password")
	if err != nil {
		t.Fatal(err)
	}
	u, err := repo.CreateUser(context.Background(), db.User{Username: "alice", PasswordHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateStorageRoot(context.Background(), db.StorageRoot{Name: "root", Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"read", "write", "archive"} {
		if _, err := repo.CreatePermission(context.Background(), db.Permission{UserID: u.ID, RootID: root.ID, PathPrefix: "/", Action: a, Allow: true}); err != nil {
			t.Fatal(err)
		}
	}
	store, err := storage.NewObjectStore(cfg.StorageRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager, err := archive.New(archive.Options{Workers: 1, MaxFiles: 10, MaxBytes: 1 << 20, TTL: time.Minute, TempDir: archiveDir}, ObjectArchiveSource{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{TTL: time.Hour})
	t.Cleanup(sessions.Close)
	limiter, err := limits.NewUploadLimiter(2, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewProductionServer(cfg, auth.NewService(repo), sessions, repo, permission.NewAuthorizer(repo, cfg.NamePolicy), store, manager, ProductionReadiness{DB: repo, ObjectStore: store, StoragePath: cfg.StorageRoot}, metrics.New(), limiter)
	if err != nil {
		t.Fatal(err)
	}
	return productionLogFixture{h, repo, root}
}
func mustHTTP(t *testing.T, c *http.Client, m, u, b string, cookie *http.Cookie) *http.Response {
	t.Helper()
	r, e := http.NewRequest(m, u, strings.NewReader(b))
	if e != nil {
		t.Fatal(e)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	resp, e := c.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	return resp
}
func readBody(t *testing.T, r *http.Response) string {
	t.Helper()
	b, e := io.ReadAll(r.Body)
	if e != nil {
		t.Fatal(e)
	}
	_ = r.Body.Close()
	return string(b)
}
func decodeLogEvents(t *testing.T, text string) []appLog.Event {
	t.Helper()
	var es []appLog.Event
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		var e appLog.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		es = append(es, e)
	}
	return es
}
func assertLifecycle(t *testing.T, es []appLog.Event, kind, id string) {
	t.Helper()
	var start, complete *appLog.Event
	for i := range es {
		if es[i].Operation == kind+".start" && es[i].TransferID == id {
			start = &es[i]
		}
		if es[i].Operation == kind+".complete" && es[i].TransferID == id {
			complete = &es[i]
		}
	}
	if start == nil || complete == nil {
		t.Fatalf("%s lifecycle for %q absent", kind, id)
	}
	if start.RequestID != complete.RequestID || start.SessionAuditID != complete.SessionAuditID {
		t.Fatalf("%s correlation mismatch", kind)
	}
}

// Cancellation is observable at the handler boundary even when the client has
// gone away before a transfer handler produces its response.
func TestTransferLifecycleLogsCanceledTerminalOutcome(t *testing.T) {
	var out bytes.Buffer
	logger := appLog.NewMulti(appLog.New(&out, appLog.FormatJSON))
	cfg := config.Default()
	h := requestContext(cfg, applicationLogWithLogger(logger, logTransferLifecycle("upload", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Err() == nil {
			t.Fatal("handler did not receive cancellation")
		}
		w.WriteHeader(http.StatusNoContent)
	}))))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodPatch, "/api/uploads/session-canceled", nil).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), r)
	events := decodeLogEvents(t, out.String())
	var terminal *appLog.Event
	for i := range events {
		if events[i].Operation == "upload.complete" {
			terminal = &events[i]
		}
	}
	if terminal == nil || terminal.Success || terminal.ErrorCode != "canceled" || terminal.Status != http.StatusNoContent {
		t.Fatalf("cancel terminal event = %#v", terminal)
	}
}
