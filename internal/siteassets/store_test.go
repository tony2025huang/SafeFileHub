package siteassets

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	im.Set(0, 0, color.Black)
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestStorePutUsesOpaqueKeyAtomicFileAndRestrictedPermissions(t *testing.T) {
	root := t.TempDir()
	s, err := New(root, Limits{MaxBytes: 1024 * 1024, MaxPixels: 100})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Put("logo.png", bytes.NewReader(pngBytes(t, 3, 4)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.Key, "site/") || strings.Contains(a.Key, "logo") || strings.Contains(a.Key, "..") {
		t.Fatalf("unsafe key %q", a.Key)
	}
	if a.ContentType != "image/png" || a.Width != 3 || a.Height != 4 {
		t.Fatalf("asset=%+v", a)
	}
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(a.Key)))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Fatalf("file permissions = %o", info.Mode().Perm())
	}
	r, err := s.Open(a.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(a.Key)))
	if err != nil || len(got) == 0 {
		t.Fatalf("content err=%v", err)
	}
}
func icoBytes(width, height byte) []byte {
	// ICO header (reserved=0, type=1, count=1), then one directory entry
	// referring to a 40-byte BITMAPINFOHEADER image payload.
	body := make([]byte, 40)
	body[0] = 40 // BITMAPINFOHEADER size, little endian
	body[4] = width
	body[8] = height * 2 // ICO bitmap height includes XOR and AND masks
	body[12] = 1         // planes
	body[14] = 32        // bits per pixel
	return append([]byte{
		0, 0, 1, 0, 1, 0,
		width, height, 0, 0, 1, 0, 32, 0, 40, 0, 0, 0, 22, 0, 0, 0,
	}, body...)
}

func TestStorePutAcceptsValidICO(t *testing.T) {
	s, err := New(t.TempDir(), Limits{MaxBytes: 1024, MaxPixels: 256 * 256})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Put("favicon.ico", bytes.NewReader(icoBytes(16, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if a.ContentType != "image/x-icon" || a.Width != 16 || a.Height != 16 {
		t.Fatalf("asset=%+v", a)
	}
	if !validKey(a.Key) || filepath.Ext(a.Key) != ".ico" {
		t.Fatalf("invalid ICO key %q", a.Key)
	}
}

func TestStoreRejectsInvalidICOAndExtensionDisguises(t *testing.T) {
	s, err := New(t.TempDir(), Limits{MaxBytes: 1024, MaxPixels: 256 * 256})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		body []byte
	}{
		{"favicon.ico", []byte("not an icon")},
		{"favicon.ico", icoBytes(0, 0)[:21]}, // entry payload lies outside the file
		{"favicon.png", icoBytes(16, 16)},
		{"favicon.ico", pngBytes(t, 1, 1)},
	}
	for _, tc := range cases {
		if _, err := s.Put(tc.name, bytes.NewReader(tc.body)); err == nil {
			t.Errorf("Put(%s) succeeded", tc.name)
		}
	}
}

func TestStoreRejectsUnsafeAssets(t *testing.T) {
	s, err := New(t.TempDir(), Limits{MaxBytes: 50, MaxPixels: 4})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		body []byte
	}{{"../logo.png", pngBytes(t, 1, 1)}, {"logo.svg", []byte("<svg></svg>")}, {"logo.png", []byte("<svg></svg>")}, {"large.png", pngBytes(t, 3, 3)}}
	for _, tc := range cases {
		if _, err := s.Put(tc.name, bytes.NewReader(tc.body)); err == nil {
			t.Errorf("Put(%s) succeeded", tc.name)
		}
	}
}

func TestRemoveOnlyAcceptsOpaqueKeysAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir, Limits{MaxBytes: 1024, MaxPixels: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Remove("../outside"); err == nil {
		t.Fatal("unsafe key accepted")
	}
	key := "site/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.png"
	if err := os.MkdirAll(filepath.Join(dir, "site"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(key)), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(key); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(key); err != nil {
		t.Fatalf("remove must be idempotent: %v", err)
	}
}
