package observability

import (
	"bytes"
	"strings"
	"testing"
)

func TestLoggerRedactsSecretsAndHasRequiredFields(t *testing.T) {
	var b bytes.Buffer
	l := New(&b, FormatJSON)
	l.Log(Event{Level: "info", Operation: "login", Route: "/login", RequestID: "r", SessionAuditID: "s", ClientIP: "1.2.3.4", PeerIP: "5.6.7.8", Status: 200, Success: true, ErrorCode: "", Fields: map[string]any{"password": "secret", "Authorization": "Bearer nope", "safe": "yes"}})
	line := b.String()
	for _, secret := range []string{"secret", "Bearer", "Authorization"} {
		if strings.Contains(line, secret) {
			t.Fatalf("secret leaked: %s", line)
		}
	}
	for _, field := range []string{"request_id", "session_audit_id", "client_ip", "peer_ip", "operation", "status", "success", "safe"} {
		if !strings.Contains(line, field) {
			t.Fatalf("missing %s: %s", field, line)
		}
	}
}
