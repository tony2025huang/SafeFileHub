package httpapi

import (
	"github.com/example/safefilehub/internal/config"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrustedProxyUsesForwardedAddressOnlyForTrustedPeer(t *testing.T) {
	cfg := config.Default()
	cfg.TrustedProxyCIDRs = []string{"10.0.0.0/8"}
	h := requestContext(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := requestClientIP(r); got != "198.51.100.9" {
			t.Fatalf("client=%s", got)
		}
		if peerIP(r) != "10.1.2.3" {
			t.Fatal("peer")
		}
	}))
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.1.2.3:5"
	r.Header.Set("X-Forwarded-For", "198.51.100.9, 10.1.2.3")
	h.ServeHTTP(httptest.NewRecorder(), r)
}
func TestTrustedProxySkipsTrustedHopsAndSupportsIPv6(t *testing.T) {
	cfg := config.Default()
	cfg.TrustedProxyCIDRs = []string{"10.0.0.0/8", "2001:db8::/32"}
	h := requestContext(cfg, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if got := requestClientIP(r); got != "198.51.100.9" {
			t.Fatalf("client=%s", got)
		}
	}))
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "[2001:db8::1]:443"
	r.Header.Set("X-Forwarded-For", "198.51.100.9, 10.2.3.4, 2001:db8::1")
	h.ServeHTTP(httptest.NewRecorder(), r)
}
func TestUntrustedPeerCannotSpoofForwardedAddress(t *testing.T) {
	cfg := config.Default()
	h := requestContext(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := requestClientIP(r); got != "203.0.113.1" {
			t.Fatalf("client=%s", got)
		}
	}))
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.1:5"
	r.Header.Set("X-Forwarded-For", "198.51.100.9")
	h.ServeHTTP(httptest.NewRecorder(), r)
}
