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

// Parse decodes raw exactly once, normalizes valid Unicode to NFC, and returns
// a canonical logical path. It never maps a logical path to a host path.
func Parse(raw string, policy config.NamePolicy) (Path, error) {
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
	if !utf8.ValidString(decoded) {
		return Path{}, fmt.Errorf("logical path is not valid UTF-8")
	}
	if containsPercentEscape(decoded) {
		return Path{}, fmt.Errorf("logical path contains double encoding")
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
	base = strings.ToUpper(base)
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" {
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		return base[3] >= '1' && base[3] <= '9'
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
