package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/example/safefilehub/internal/config"
)

func TestServerServesEmbeddedTransferUI(t *testing.T) {
	h, err := NewServer(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ path, contentType, body string }{
		{"/", "text/html", "app.js"},
		{"/index.html", "text/html", "app.js"},
		{"/app.js", "text/javascript", "createUploadBatch"},
	} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, want.path, nil))
		if r.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", want.path, r.Code)
			continue
		}
		if !strings.Contains(r.Header().Get("Content-Type"), want.contentType) {
			t.Errorf("GET %s Content-Type = %q, want %q", want.path, r.Header().Get("Content-Type"), want.contentType)
		}
		if !strings.Contains(r.Body.String(), want.body) {
			t.Errorf("GET %s did not serve expected asset", want.path)
		}
		if strings.Contains(r.Body.String(), config.Default().StorageRoot) {
			t.Errorf("GET %s leaked storage root", want.path)
		}
	}
}

func TestServerLoginPageIsSelfContainedAndPublic(t *testing.T) {
	h, err := NewServer(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/login", "/login.html"} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
		if r.Code != http.StatusOK || strings.Contains(r.Body.String(), `src="./app.js"`) {
			t.Fatalf("login page is not a public self-contained page: status=%d body=%q", r.Code, r.Body.String())
		}
	}
}

func TestServerStaticUIOnlyAcceptsKnownAssets(t *testing.T) {
	h, err := NewServer(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/web/index.html", nil))
	if r.Code != http.StatusNotFound {
		t.Fatalf("GET /web/index.html status = %d, want 404", r.Code)
	}
}
