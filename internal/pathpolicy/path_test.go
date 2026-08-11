package pathpolicy

import (
	"reflect"
	"testing"

	"github.com/example/safefilehub/internal/config"
)

func TestParseCanonicalizesLogicalPaths(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		canonical string
		segments  []string
	}{
		{name: "root", raw: "/", canonical: "/", segments: nil},
		{name: "nested", raw: "projects/2026/report.txt", canonical: "/projects/2026/report.txt", segments: []string{"projects", "2026", "report.txt"}},
		{name: "one percent decode", raw: "reports%20and%20notes/100%25%20ready", canonical: "/reports and notes/100% ready", segments: []string{"reports and notes", "100% ready"}},
		{name: "unicode NFC", raw: "cafe%CC%81/%E4%B8%AD%E6%96%87%F0%9F%93%81", canonical: "/café/中文📁", segments: []string{"café", "中文📁"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.raw, config.Default().NamePolicy)
			if err != nil {
				t.Fatalf("Parse(%q) error = %v", tc.raw, err)
			}
			if got.Canonical != tc.canonical {
				t.Errorf("Canonical = %q, want %q", got.Canonical, tc.canonical)
			}
			if !reflect.DeepEqual(got.Segments, tc.segments) {
				t.Errorf("Segments = %#v, want %#v", got.Segments, tc.segments)
			}
		})
	}
}

func TestParseRejectsUnsafeLogicalPaths(t *testing.T) {
	tests := []string{
		"../secret",
		"safe/../secret",
		"%2e%2e/secret",
		"safe/%2E%2e/secret",
		"%252e%252e/secret",
		"safe%255csecret",
		"safe\\secret",
		"safe%00name",
		"safe%0Aname",
		"safe%7Fname",
		"/etc/passwd",
		"C:/Windows",
		"z:%5CWindows",
		"safe//child",
		"safe/",
		"./safe",
		"%",
	}

	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			if _, err := Parse(raw, config.Default().NamePolicy); err == nil {
				t.Fatalf("Parse(%q) succeeded, want error", raw)
			}
		})
	}
}

func TestParseAppliesDefaultFilenamePolicy(t *testing.T) {
	for _, raw := range []string{
		".hidden", "~backup", "$tmp", "#draft",
		"CON", "prn.txt", "aux", "NUL", "COM1", "com9.log", "LPT1", "lpt9.tmp",
		"trailing ", "trailing.",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := Parse(raw, config.Default().NamePolicy); err == nil {
				t.Fatalf("Parse(%q) succeeded, want error", raw)
			}
		})
	}
}

func TestParseAllowsSafeSpecialAndUnicodeNames(t *testing.T) {
	got, err := Parse("plus+percent%25-question%3F/%E4%B8%AD%E6%96%87%F0%9F%98%80", config.Default().NamePolicy)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if want := "/plus+percent%-question?/中文😀"; got.Canonical != want {
		t.Fatalf("Canonical = %q, want %q", got.Canonical, want)
	}
}
