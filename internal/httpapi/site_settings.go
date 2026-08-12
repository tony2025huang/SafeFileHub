package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/siteassets"
)

// siteSettingsRepository is deliberately limited to metadata needed by the
// public branding surface. Opaque filesystem keys never leave this boundary.
type siteSettingsRepository interface {
	adminRepository
	SiteSettings(context.Context) (db.SiteSettings, error)
	UpdateSiteSettingsWithAudit(context.Context, db.SiteSettings, db.AuditEvent) error
	ReplaceSiteAssetWithAudit(context.Context, string, db.SiteAsset, db.AuditEvent) (db.SiteAsset, error)
	ResetSiteAssetWithAudit(context.Context, string, db.AuditEvent) error
	PublicSiteAssetByID(context.Context, int64) (db.SiteAsset, error)
	SiteAssetCleanupKeys(context.Context, int) ([]string, error)
	CompleteSiteAssetCleanup(context.Context, string) error
}
type removableSiteAssetStore interface {
	siteAssetStore
	Remove(string) error
}
type siteAssetStore interface {
	Put(string, io.Reader) (siteassets.Asset, error)
	Open(string) (io.ReadCloser, error)
}

type siteSettingsResponse struct {
	SiteName      string `json:"site_name"`
	PrimaryColor  string `json:"primary_color"`
	FilingEnabled bool   `json:"filing_enabled"`
	FilingText    string `json:"filing_text,omitempty"`
	MD5Enabled    bool   `json:"md5_enabled"`
	LoginLogoURL  string `json:"login_logo_url,omitempty"`
	NavLogoURL    string `json:"nav_logo_url,omitempty"`
	FaviconURL    string `json:"favicon_url,omitempty"`
}

type siteSettingsRequest struct {
	SiteName      string `json:"site_name"`
	PrimaryColor  string `json:"primary_color"`
	FilingEnabled bool   `json:"filing_enabled"`
	FilingText    string `json:"filing_text"`
	MD5Enabled    bool   `json:"md5_enabled"`
}
type siteAssetResponse struct {
	ID          int64  `json:"id"`
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	Width       int64  `json:"width"`
	Height      int64  `json:"height"`
}

func NewServerWithSiteSettings(cfg config.Config, users authenticator, sessions userSessionRevoker, repo siteSettingsRepository, assets siteAssetStore) (http.Handler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if users == nil || sessions == nil || repo == nil || assets == nil {
		return nil, errors.New("site settings dependencies are required")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("POST /login", login(users, sessions))
	mux.HandleFunc("POST /logout", logout(sessions))
	mux.Handle("GET /session", requireSession(sessions, http.HandlerFunc(sessionStatus)))
	registerSiteSettingsRoutes(mux, cfg, sessions, repo, assets)
	return RequestLimits(cfg, mux), nil
}
func registerSiteSettingsRoutes(mux *http.ServeMux, cfg config.Config, sessions userSessionRevoker, repo siteSettingsRepository, assets siteAssetStore) {
	mux.HandleFunc("GET /api/site-settings", getPublicSiteSettings(repo))
	mux.HandleFunc("GET /assets/site/{assetID}", serveSiteAsset(repo, assets))
	mux.HandleFunc("GET /favicon.ico", serveFavicon(repo, assets))
	guard := func(next http.Handler) http.Handler { return requireSession(sessions, requireAdmin(cfg, repo, next)) }
	mux.Handle("GET /api/admin/site-settings", guard(http.HandlerFunc(getAdminSiteSettings(repo))))
	mux.Handle("PUT /api/admin/site-settings", guard(http.HandlerFunc(putAdminSiteSettings(repo))))
	mux.Handle("POST /api/admin/site-settings/assets/{kind}", guard(http.HandlerFunc(uploadSiteAsset(repo, assets))))
	mux.Handle("DELETE /api/admin/site-settings/assets/{kind}", guard(http.HandlerFunc(resetSiteAsset(repo, assets))))
}
func siteSettingsPublic(s db.SiteSettings) siteSettingsResponse {
	r := siteSettingsResponse{SiteName: s.SiteName, PrimaryColor: s.PrimaryColor, FilingEnabled: s.FilingEnabled, MD5Enabled: s.MD5Enabled}
	if s.FilingEnabled {
		r.FilingText = s.FilingText
	}
	if s.LoginLogoAssetID > 0 {
		r.LoginLogoURL = siteAssetURL(s.LoginLogoAssetID)
	}
	if s.NavLogoAssetID > 0 {
		r.NavLogoURL = siteAssetURL(s.NavLogoAssetID)
	}
	if s.FaviconAssetID > 0 {
		r.FaviconURL = siteAssetURL(s.FaviconAssetID)
	}
	return r
}
func siteAssetURL(id int64) string { return "/assets/site/" + strconv.FormatInt(id, 10) }
func writeSiteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
func getPublicSiteSettings(repo siteSettingsRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, e := repo.SiteSettings(r.Context())
		if e != nil {
			http.Error(w, "site settings unavailable", 500)
			return
		}
		writeSiteJSON(w, siteSettingsPublic(s))
	}
}
func getAdminSiteSettings(repo siteSettingsRepository) http.HandlerFunc {
	return getPublicSiteSettings(repo)
}
func putAdminSiteSettings(repo siteSettingsRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in siteSettingsRequest
		if e := decodeAdminJSON(w, r, &in); e != nil {
			http.Error(w, "invalid site settings", 400)
			return
		}
		s := db.SiteSettings{SiteName: strings.TrimSpace(in.SiteName), PrimaryColor: in.PrimaryColor, FilingEnabled: in.FilingEnabled, FilingText: strings.TrimSpace(in.FilingText), MD5Enabled: in.MD5Enabled}
		old, e := repo.SiteSettings(r.Context())
		if e != nil {
			http.Error(w, "site settings unavailable", 500)
			return
		}
		s.LoginLogoAssetID, s.NavLogoAssetID, s.FaviconAssetID = old.LoginLogoAssetID, old.NavLogoAssetID, old.FaviconAssetID
		e = repo.UpdateSiteSettingsWithAudit(r.Context(), s, db.AuditEvent{UserID: adminActor(r), Action: "admin.site_settings.update", LogicalPath: "/", Detail: "target_user_id=0"})
		if e != nil {
			if isInvalidSiteSettings(e) {
				http.Error(w, "invalid site settings", http.StatusBadRequest)
			} else {
				http.Error(w, "site settings unavailable", http.StatusInternalServerError)
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
func uploadSiteAsset(repo siteSettingsRepository, store siteAssetStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		kind := r.PathValue("kind")
		filename, ok := assetFilename(kind, r.Header.Get("Content-Type"))
		if !ok {
			http.Error(w, "invalid site asset", 400)
			return
		}
		a, e := store.Put(filename, http.MaxBytesReader(w, r.Body, 8<<20))
		if e != nil {
			if errors.Is(e, siteassets.ErrInvalidAsset) {
				http.Error(w, "invalid site asset", http.StatusBadRequest)
			} else {
				http.Error(w, "store site asset", http.StatusInternalServerError)
			}
			return
		}
		candidate := db.SiteAsset{OpaqueKey: a.Key, ContentType: a.ContentType, Size: a.Size, Width: a.Width, Height: a.Height}
		saved, e := repo.ReplaceSiteAssetWithAudit(r.Context(), kind, candidate, db.AuditEvent{UserID: adminActor(r), Action: "admin.site_asset.upload", LogicalPath: "/" + kind, Detail: "target_user_id=0"})
		if e != nil {
			if removable, ok := store.(removableSiteAssetStore); ok {
				_ = removable.Remove(a.Key)
			}
			http.Error(w, "save site asset", 500)
			return
		}
		cleanupSiteAssets(r.Context(), repo, store)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(siteAssetResponse{ID: saved.ID, URL: siteAssetURL(saved.ID), ContentType: saved.ContentType, Size: saved.Size, Width: saved.Width, Height: saved.Height})
	}
}
func resetSiteAsset(repo siteSettingsRepository, store siteAssetStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		kind := r.PathValue("kind")
		if !validSiteAssetKind(kind) {
			http.Error(w, "invalid site asset", http.StatusBadRequest)
			return
		}
		if err := repo.ResetSiteAssetWithAudit(r.Context(), kind, db.AuditEvent{UserID: adminActor(r), Action: "admin.site_asset.reset", LogicalPath: "/" + kind, Detail: "target_user_id=0"}); err != nil {
			http.Error(w, "reset site asset", http.StatusInternalServerError)
			return
		}
		cleanupSiteAssets(r.Context(), repo, store)
		w.WriteHeader(http.StatusNoContent)
	}
}
func validSiteAssetKind(kind string) bool {
	return kind == "login_logo" || kind == "nav_logo" || kind == "favicon"
}
func assetFilename(kind, contentType string) (string, bool) {
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if !validSiteAssetKind(kind) {
		return "", false
	}
	ext := map[string]string{"image/png": "png", "image/jpeg": "jpeg", "image/gif": "gif", "image/x-icon": "ico", "image/vnd.microsoft.icon": "ico"}[contentType]
	if ext == "" {
		return "", false
	}
	if kind == "favicon" && ext != "ico" && ext != "png" {
		return "", false
	}
	return kind + "." + ext, true
}
func serveSiteAsset(repo siteSettingsRepository, store siteAssetStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, e := strconv.ParseInt(r.PathValue("assetID"), 10, 64)
		if e != nil || id <= 0 {
			http.NotFound(w, r)
			return
		}
		serveSiteAssetID(w, r, repo, store, id)
	}
}
func serveFavicon(repo siteSettingsRepository, store siteAssetStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, e := repo.SiteSettings(r.Context())
		if e != nil || s.FaviconAssetID <= 0 {
			http.NotFound(w, r)
			return
		}
		serveSiteAssetID(w, r, repo, store, s.FaviconAssetID)
	}
}
func serveSiteAssetID(w http.ResponseWriter, r *http.Request, repo siteSettingsRepository, store siteAssetStore, id int64) {
	a, e := repo.PublicSiteAssetByID(r.Context(), id)
	if e != nil {
		http.NotFound(w, r)
		return
	}
	f, e := store.Open(a.OpaqueKey)
	if e != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", a.ContentType)
	if r.URL.Path == "/favicon.ico" {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.FormatInt(a.Size, 10))
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, f)
	}
}

func isInvalidSiteSettings(err error) bool {
	return err != nil && strings.Contains(err.Error(), "invalid site settings")
}
func cleanupSiteAssets(ctx context.Context, repo siteSettingsRepository, store siteAssetStore) {
	removable, ok := store.(removableSiteAssetStore)
	if !ok {
		return
	}
	_, _ = siteassets.RecoverCleanup(ctx, repo, removable, 16)
}
