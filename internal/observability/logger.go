// Package observability provides redacted, parseable application logging.
package observability

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

type Format string

const (
	FormatJSON Format = "json"
	FormatText Format = "text"
)

type Event struct {
	Time           time.Time      `json:"time"`
	Level          string         `json:"level"`
	ClientIP       string         `json:"client_ip"`
	PeerIP         string         `json:"peer_ip"`
	UserID         int64          `json:"user_id,omitempty"`
	RequestID      string         `json:"request_id"`
	SessionAuditID string         `json:"session_audit_id,omitempty"`
	TransferID     string         `json:"transfer_id,omitempty"`
	Operation      string         `json:"operation"`
	Route          string         `json:"route"`
	Success        bool           `json:"success"`
	Status         int            `json:"status"`
	ErrorCode      string         `json:"error_code,omitempty"`
	Bytes          int64          `json:"bytes"`
	DurationMS     int64          `json:"duration_ms"`
	Fields         map[string]any `json:"-"`
}
type Logger struct {
	out    io.Writer
	format Format
	mu     sync.Mutex
}

func New(out io.Writer, format Format) *Logger {
	if format != FormatText {
		format = FormatJSON
	}
	return &Logger{out: out, format: format}
}
func (l *Logger) Log(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	e.Fields = redact(e.Fields)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.format == FormatJSON {
		m := map[string]any{"time": e.Time.Format(time.RFC3339Nano), "level": e.Level, "client_ip": e.ClientIP, "peer_ip": e.PeerIP, "request_id": e.RequestID, "session_audit_id": e.SessionAuditID, "transfer_id": e.TransferID, "operation": e.Operation, "route": e.Route, "success": e.Success, "status": e.Status, "error_code": e.ErrorCode, "bytes": e.Bytes, "duration_ms": e.DurationMS}
		if e.UserID > 0 {
			m["user_id"] = e.UserID
		}
		for k, v := range e.Fields {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		_, _ = l.out.Write(append(b, '\n'))
		return
	}
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{fmt.Sprintf("time=%q", e.Time.Format(time.RFC3339Nano)), "level=" + e.Level, "operation=" + e.Operation, "route=" + e.Route, fmt.Sprintf("status=%d success=%t", e.Status, e.Success), "request_id=" + e.RequestID, "client_ip=" + e.ClientIP, "peer_ip=" + e.PeerIP}
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, fmt.Sprint(e.Fields[k])))
	}
	_, _ = fmt.Fprintln(l.out, strings.Join(parts, " "))
}
func redact(in map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		low := strings.ToLower(k)
		if strings.Contains(low, "password") || strings.Contains(low, "cookie") || strings.Contains(low, "authorization") || strings.Contains(low, "token") || strings.Contains(low, "content") || strings.Contains(low, "query") {
			continue
		}
		out[k] = v
	}
	return out
}
func Standard(format Format) *Logger { return New(log.Writer(), format) }

// MultiLogger mirrors every redacted application event to each configured sink.
type MultiLogger struct{ loggers []*Logger }

func NewMulti(loggers ...*Logger) *MultiLogger { return &MultiLogger{loggers: loggers} }
func (l *MultiLogger) Log(e Event) {
	for _, logger := range l.loggers {
		if logger != nil {
			logger.Log(e)
		}
	}
}
