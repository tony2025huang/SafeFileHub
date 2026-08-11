package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/safefilehub/internal/auth"
	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/siteassets"
)

func TestSiteSettingsPublicAdminAndAssets(t *testing.T) {
	ctx := context.Background()
	repo, err := db.Open(ctx, filepath.Join(t.TempDir(), "site.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	hash, _ := auth.HashPassword("password")
	admin, err := repo.CreateUser(ctx, db.User{Username: "admin", PasswordHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	assets, err := siteassets.New(t.TempDir(), siteassets.Limits{MaxBytes: 1024 * 1024, MaxPixels: 10000})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AdminUsernames = []string{"admin"}
	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{TTL: time.Hour, InsecureCookie: true})
	defer sessions.Close()
	h, err := NewServerWithSiteSettings(cfg, auth.NewService(repo), sessions, repo, assets)
	if err != nil {
		t.Fatal(err)
	}

	public := siteRequest(t, h, nil, http.MethodGet, "/api/site-settings", "", "")
	if public.Code != http.StatusOK {
		t.Fatalf("public status=%d: %s", public.Code, public.Body.String())
	}
	if strings.Contains(public.Body.String(), "opaque_key") || strings.Contains(public.Body.String(), "storage") {
		t.Fatalf("public settings leaked internals: %s", public.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(public.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["site_name"] == "" || got["primary_color"] == "" || got["md5_enabled"] == nil {
		t.Fatalf("incomplete public settings: %#v", got)
	}

	cookie := adminSession(t, sessions, admin.ID)
	update := siteRequest(t, h, cookie, http.MethodPut, "/api/admin/site-settings", `{"site_name":"Example files","primary_color":"#123abc","filing_enabled":true,"filing_text":"ICP 123","md5_enabled":true}`, "application/json")
	if update.Code != http.StatusNoContent {
		t.Fatalf("update=%d: %s", update.Code, update.Body.String())
	}
	bad := siteRequest(t, h, cookie, http.MethodPut, "/api/admin/site-settings", `{"site_name":"Example","primary_color":"red"}`, "application/json")
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("bad settings=%d", bad.Code)
	}

	image := pngBytes(t, 2, 2)
	upload := siteRequest(t, h, cookie, http.MethodPost, "/api/admin/site-settings/assets/login_logo", string(image), "image/png")
	if upload.Code != http.StatusCreated {
		t.Fatalf("upload=%d: %s", upload.Code, upload.Body.String())
	}
	var asset struct {
		ID        int64  `json:"id"`
		URL       string `json:"url"`
		OpaqueKey string `json:"opaque_key"`
	}
	if err := json.NewDecoder(upload.Body).Decode(&asset); err != nil {
		t.Fatal(err)
	}
	if asset.ID <= 0 || asset.URL != "/assets/site/"+itoa(asset.ID) || asset.OpaqueKey != "" {
		t.Fatalf("unsafe asset response: %#v", asset)
	}

	file := siteRequest(t, h, nil, http.MethodGet, asset.URL, "", "")
	if file.Code != http.StatusOK || file.Header().Get("Content-Type") != "image/png" || file.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" || !bytes.Equal(file.Body.Bytes(), image) {
		t.Fatalf("asset=%d content-type=%q body=%d", file.Code, file.Header().Get("Content-Type"), file.Body.Len())
	}
	favicon := siteRequest(t, h, cookie, http.MethodPost, "/api/admin/site-settings/assets/favicon", string(icoBytes(16, 16)), "image/x-icon")
	if favicon.Code != http.StatusCreated {
		t.Fatalf("favicon upload=%d: %s", favicon.Code, favicon.Body.String())
	}
	redirect := siteRequest(t, h, nil, http.MethodGet, "/favicon.ico", "", "")
	if redirect.Code != http.StatusOK || redirect.Header().Get("Content-Type") != "image/x-icon" || redirect.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("favicon=%d content-type=%q", redirect.Code, redirect.Header().Get("Content-Type"))
	}
	for _, kind := range []string{"login_logo", "favicon", "nav_logo"} {
		if kind == "nav_logo" {
			if r := siteRequest(t, h, cookie, http.MethodPost, "/api/admin/site-settings/assets/nav_logo", string(image), "image/png"); r.Code != http.StatusCreated {
				t.Fatalf("nav upload=%d: %s", r.Code, r.Body.String())
			}
		}
		reset := siteRequest(t, h, cookie, http.MethodDelete, "/api/admin/site-settings/assets/"+kind, "", "")
		if reset.Code != http.StatusNoContent {
			t.Fatalf("reset %s=%d: %s", kind, reset.Code, reset.Body.String())
		}
	}
	settings, err := repo.SiteSettings(ctx)
	if err != nil || settings.LoginLogoAssetID != 0 || settings.NavLogoAssetID != 0 || settings.FaviconAssetID != 0 {
		t.Fatalf("reset settings=%#v, %v", settings, err)
	}
	if r := siteRequest(t, h, nil, http.MethodGet, "/favicon.ico", "", ""); r.Code != http.StatusNotFound {
		t.Fatalf("reset favicon=%d", r.Code)
	}
	if r := siteRequest(t, h, cookie, http.MethodDelete, "/api/admin/site-settings/assets/svg", "", ""); r.Code != http.StatusBadRequest {
		t.Fatalf("invalid reset=%d", r.Code)
	}

	events, err := repo.AuditEventsForUser(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 6 {
		t.Fatalf("audit event count = %d, want >= 6", len(events))
	}
	for _, event := range events {
		if strings.Contains(event.Detail, "site/") {
			t.Fatalf("audit leaked asset key: %#v", event)
		}
	}
}

type failingSiteAssetStore struct{ err error }

func (s failingSiteAssetStore) Put(string, io.Reader) (siteassets.Asset, error) {
	return siteassets.Asset{}, s.err
}
func (s failingSiteAssetStore) Open(string) (io.ReadCloser, error) { return nil, s.err }

func TestSiteAssetStorageFailureIsServerError(t *testing.T) {
	ctx := context.Background()
	repo, err := db.Open(ctx, filepath.Join(t.TempDir(), "site.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	hash, _ := auth.HashPassword("password")
	admin, err := repo.CreateUser(ctx, db.User{Username: "admin", PasswordHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AdminUsernames = []string{"admin"}
	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{InsecureCookie: true})
	defer sessions.Close()
	h, err := NewServerWithSiteSettings(cfg, auth.NewService(repo), sessions, repo, failingSiteAssetStore{err: errors.New("disk offline")})
	if err != nil {
		t.Fatal(err)
	}
	r := siteRequest(t, h, adminSession(t, sessions, admin.ID), http.MethodPost, "/api/admin/site-settings/assets/login_logo", "payload", "image/png")
	if r.Code != http.StatusInternalServerError {
		t.Fatalf("storage failure=%d: %s", r.Code, r.Body.String())
	}
}

func TestSiteSettingsAdminAndUploadValidation(t *testing.T) {
	ctx := context.Background()
	repo, err := db.Open(ctx, filepath.Join(t.TempDir(), "site.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	hash, _ := auth.HashPassword("password")
	if _, err := repo.CreateUser(ctx, db.User{Username: "initial", PasswordHash: hash}); err != nil {
		t.Fatal(err)
	}
	member, _ := repo.CreateUser(ctx, db.User{Username: "member", PasswordHash: hash})
	assets, _ := siteassets.New(t.TempDir(), siteassets.Limits{MaxBytes: 32, MaxPixels: 100})
	cfg := config.Default()
	cfg.AdminUsernames = []string{"admin"}
	sessions := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{InsecureCookie: true})
	defer sessions.Close()
	h, err := NewServerWithSiteSettings(cfg, auth.NewService(repo), sessions, repo, assets)
	if err != nil {
		t.Fatal(err)
	}
	cookie := adminSession(t, sessions, member.ID)
	if r := siteRequest(t, h, cookie, http.MethodGet, "/api/admin/site-settings", "", ""); r.Code != http.StatusForbidden {
		t.Fatalf("non-admin=%d", r.Code)
	}
	if r := siteRequest(t, h, cookie, http.MethodPost, "/api/admin/site-settings/assets/nope", "x", "image/png"); r.Code != http.StatusForbidden {
		t.Fatalf("non-admin=%d", r.Code)
	}
}
func siteRequest(t *testing.T, h http.Handler, cookie *http.Cookie, method, target, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func pngBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	var b bytes.Buffer
	im := image.NewRGBA(image.Rect(0, 0, width, height))
	im.Set(0, 0, color.Black)
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func icoBytes(width, height byte) []byte {
	body := make([]byte, 40)
	body[0] = 40
	body[4] = width
	body[8] = height * 2
	body[12] = 1
	body[14] = 32
	return append([]byte{0, 0, 1, 0, 1, 0, width, height, 0, 0, 1, 0, 32, 0, 40, 0, 0, 0, 22, 0, 0, 0}, body...)
}
