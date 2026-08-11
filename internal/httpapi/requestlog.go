package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"github.com/example/safefilehub/internal/config"
	"net"
	"net/http"
	"strings"
)

type requestIDKey struct{}
type clientIPKey struct{}
type peerIPKey struct{}
type sessionAuditIDKey struct{}
type transferIDKey struct{}
type applicationLoggerKey struct{}
type requestAuditStateKey struct{}

type requestAuditState struct {
	userID         int64
	sessionAuditID string
}

func randomID() string {
	b := make([]byte, 18)
	if _, e := rand.Read(b); e != nil {
		return "unavailable"
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func requestContext(cfg config.Config, next http.Handler) http.Handler {
	trusted := parseCIDRs(cfg.TrustedProxyCIDRs)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer := host(r.RemoteAddr)
		client := peer
		if trustedPeer(peer, trusted) {
			// Work from the proxy-facing end of XFF. A trusted proxy appends its
			// direct client; skipping trusted hops prevents a client-supplied left
			// value from winning when several trusted proxies are chained.
			client = forwarded(r, trusted)
			if client == "" {
				if real := host(r.Header.Get("X-Real-IP")); net.ParseIP(real) != nil {
					client = real
				}
			}
			if client == "" {
				client = peer
			}
		}
		ctx := context.WithValue(r.Context(), requestIDKey{}, randomID())
		ctx = context.WithValue(ctx, peerIPKey{}, peer)
		ctx = context.WithValue(ctx, clientIPKey{}, client)
		ctx = context.WithValue(ctx, requestAuditStateKey{}, &requestAuditState{})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
func parseCIDRs(values []string) []*net.IPNet {
	var out []*net.IPNet
	for _, v := range values {
		if _, n, e := net.ParseCIDR(v); e == nil {
			out = append(out, n)
		}
	}
	return out
}
func trustedPeer(peer string, nets []*net.IPNet) bool {
	ip := net.ParseIP(peer)
	for _, n := range nets {
		if ip != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}
func host(v string) string {
	h, _, e := net.SplitHostPort(v)
	if e == nil {
		return h
	}
	return strings.Trim(v, "[]")
}
func forwarded(r *http.Request, trusted []*net.IPNet) string {
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(parts[i]))
		if ip != nil && !trustedPeer(ip.String(), trusted) {
			return ip.String()
		}
	}
	return ""
}
func requestClientIP(r *http.Request) string {
	v, _ := r.Context().Value(clientIPKey{}).(string)
	if v != "" {
		return v
	}
	ip, _ := clientIP(r)
	return ip
}
func peerIP(r *http.Request) string    { v, _ := r.Context().Value(peerIPKey{}).(string); return v }
func requestID(r *http.Request) string { v, _ := r.Context().Value(requestIDKey{}).(string); return v }
func sessionAuditID(r *http.Request) string {
	v, _ := r.Context().Value(sessionAuditIDKey{}).(string)
	if v != "" {
		return v
	}
	state, _ := r.Context().Value(requestAuditStateKey{}).(*requestAuditState)
	if state != nil {
		return state.sessionAuditID
	}
	return ""
}
func transferID(r *http.Request) string {
	if v, _ := r.Context().Value(transferIDKey{}).(string); v != "" {
		return v
	}
	if id := r.PathValue("id"); id != "" {
		return id
	}
	if id := r.PathValue("fileID"); id != "" {
		return "file-" + id
	}
	if id := r.PathValue("jobID"); id != "" {
		return "archive-" + id
	}
	// Archive creation has no job ID until its handler succeeds. It must not use
	// a request-scoped substitute: createArchive emits the lifecycle only after
	// the manager returns its durable archive-<jobID> identifier.
	return ""
}
func sessionAuditToken(value string) string {
	sum := sha256.Sum256([]byte(value))
	return base64.RawURLEncoding.EncodeToString(sum[:12])
}
