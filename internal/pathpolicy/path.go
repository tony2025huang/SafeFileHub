// Package pathpolicy validates user-facing logical paths independently from
// physical storage object keys.
package pathpolicy

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/example/safefilehub/internal/config"
	"golang.org/x/text/unicode/norm"
)

// Path is a validated logical path. Canonical always begins with a slash.
type Path struct {
	Canonical string
	Segments  []string
}

// ParseEscapedPath decodes an escaped, relative logical path exactly once,
// normalizes valid Unicode to NFC, and returns a canonical logical path.
//
// Callers that receive an HTTP request must pass r.URL.EscapedPath(), not
// r.URL.Path: net/http has already decoded URL.Path. For already-decoded input,
// use ParseDecodedPath instead. Neither function maps a logical path to a host
// path.
func ParseEscapedPath(raw string, policy config.NamePolicy) (Path, error) {
	if raw == "/" {
		return Path{Canonical: "/"}, nil
	}
	if raw == "" || strings.HasPrefix(raw, "/") {
		return Path{}, fmt.Errorf("logical path must be relative or root")
	}

	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return Path{}, fmt.Errorf("decode logical path: %w", err)
	}
	if containsPercentEscape(decoded) {
		return Path{}, fmt.Errorf("logical path contains double encoding")
	}
	return parseDecodedPath(decoded, policy)
}

// ParseDecodedPath validates a relative logical path that has already been URL
// decoded. Literal percent signs, including text such as "%2E", are filenames
// here and are not decoded again.
func ParseDecodedPath(decoded string, policy config.NamePolicy) (Path, error) {
	if decoded == "/" {
		return Path{Canonical: "/"}, nil
	}
	if decoded == "" || strings.HasPrefix(decoded, "/") {
		return Path{}, fmt.Errorf("logical path must be relative or root")
	}
	return parseDecodedPath(decoded, policy)
}

// Parse is kept for compatibility. New callers must use ParseEscapedPath or
// ParseDecodedPath to make their URL-decoding boundary explicit.
// Deprecated: use ParseEscapedPath or ParseDecodedPath.
func Parse(raw string, policy config.NamePolicy) (Path, error) {
	return ParseEscapedPath(raw, policy)
}

func parseDecodedPath(decoded string, policy config.NamePolicy) (Path, error) {
	if !utf8.ValidString(decoded) {
		return Path{}, fmt.Errorf("logical path is not valid UTF-8")
	}

	segments := strings.Split(norm.NFC.String(decoded), "/")
	for _, segment := range segments {
		if err := validateSegment(segment, policy); err != nil {
			return Path{}, err
		}
	}
	return Path{Canonical: "/" + strings.Join(segments, "/"), Segments: segments}, nil
}

func validateSegment(segment string, policy config.NamePolicy) error {
	if segment == "" {
		return fmt.Errorf("logical path contains an empty segment")
	}
	if segment == "." || segment == ".." {
		return fmt.Errorf("logical path contains traversal segment")
	}
	if strings.ContainsRune(segment, '\\') {
		return fmt.Errorf("logical path contains backslash")
	}
	if hasWindowsDrivePrefix(segment) {
		return fmt.Errorf("logical path contains Windows drive path")
	}
	if strings.HasSuffix(segment, " ") || strings.HasSuffix(segment, ".") {
		return fmt.Errorf("logical path segment has a trailing space or dot")
	}
	for _, r := range segment {
		if r == 0 || unicode.IsControl(r) {
			return fmt.Errorf("logical path contains control character")
		}
	}
	if isWindowsReservedName(segment) {
		return fmt.Errorf("logical path contains Windows reserved name")
	}
	if rejectsLeading(segment, policy) {
		return fmt.Errorf("logical path segment has forbidden leading character")
	}
	return nil
}

func containsPercentEscape(s string) bool {
	for i := 0; i+2 < len(s); i++ {
		if s[i] == '%' && isHex(s[i+1]) && isHex(s[i+2]) {
			return true
		}
	}
	return false
}

func isHex(b byte) bool {
	return ('0' <= b && b <= '9') || ('a' <= b && b <= 'f') || ('A' <= b && b <= 'F')
}

func hasWindowsDrivePrefix(s string) bool {
	return len(s) >= 2 && ((s[0] >= 'A' && s[0] <= 'Z') || (s[0] >= 'a' && s[0] <= 'z')) && s[1] == ':'
}

func isWindowsReservedName(s string) bool {
	base := s
	if dot := strings.IndexByte(base, '.'); dot >= 0 {
		base = base[:dot]
	}

	// Windows device names are case-insensitive. EqualFold applies Unicode case
	// folding, so compatibility characters such as the Kelvin sign are not able
	// to evade this check through case conversion.
	for _, reserved := range []string{
		"CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9",
		// Conservative aliases for Kelvin-sign case-folding forms. These are
		// rejected alongside the Windows device namespace to avoid platform
		// specific interpretation differences.
		"KON", "KOM1", "KOM2", "KOM3", "KOM4", "KOM5", "KOM6", "KOM7", "KOM8", "KOM9",
		"KPT1", "KPT2", "KPT3", "KPT4", "KPT5", "KPT6", "KPT7", "KPT8", "KPT9",
	} {
		if strings.EqualFold(base, reserved) {
			return true
		}
	}
	return false
}

func rejectsLeading(s string, policy config.NamePolicy) bool {
	if s == "" {
		return false
	}
	switch s[0] {
	case '.':
		return policy.RejectLeadingDot
	case '~':
		return policy.RejectLeadingTilde
	case '$':
		return policy.RejectLeadingDollar
	case '#':
		return policy.RejectLeadingHash
	default:
		return false
	}
}
