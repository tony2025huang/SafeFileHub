package httpapi

import (
	"embed"
	"net/http"
	"path"
	"strings"
)

//go:embed assets/index.html assets/app.js
var staticAssets embed.FS

// transferUI serves only the two compiled, embedded public assets. It does not
// expose the server working directory or any host filesystem path.
func transferUI(w http.ResponseWriter, r *http.Request) {
	asset := ""
	switch path.Clean(r.URL.Path) {
	case ".", "/", "/index.html":
		asset = "assets/index.html"
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case "/app.js":
		asset = "assets/app.js"
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	default:
		http.NotFound(w, r)
		return
	}
	body, err := staticAssets.ReadFile(asset)
	if err != nil { // embedded assets are built with the binary; do not reveal details.
		http.NotFound(w, r)
		return
	}
	// Immutable content is intentional: app.js references no user data.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if strings.HasSuffix(asset, ".html") {
		w.Header().Set("Cache-Control", "no-cache")
	}
	_, _ = w.Write(body)
}
