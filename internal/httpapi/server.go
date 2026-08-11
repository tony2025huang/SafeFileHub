// Package httpapi exposes SafeFileHub's HTTP endpoints.
package httpapi

import (
	"fmt"
	"net/http"

	"github.com/example/safefilehub/internal/config"
)

// NewServer returns an in-memory HTTP handler configured for SafeFileHub.
func NewServer(cfg config.Config) (http.Handler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate server configuration: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	return mux, nil
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}
