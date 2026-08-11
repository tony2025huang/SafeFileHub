// SafeFileHub serves files from configured logical storage roots.
package main

import (
	"log"
	"net/http"

	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/httpapi"
)

func main() {
	cfg := config.Default()
	h, err := httpapi.NewServer(cfg)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("SafeFileHub listening on %s", cfg.ListenAddr)
	log.Fatal(http.ListenAndServe(cfg.ListenAddr, h))
}
