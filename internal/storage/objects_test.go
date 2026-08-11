package storage

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestObjectCreationUsesRandomKeysRatherThanLogicalPaths(t *testing.T) {
	store, err := NewObjectStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	key, writer, err := store.Create("/reports/../../outside.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, "safe"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "objects/") || strings.Contains(key, "reports") || strings.Contains(key, "outside") || strings.Contains(key, "..") {
		t.Fatalf("object key %q exposes logical path", key)
	}
	reader, err := store.Open(key)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil || string(got) != "safe" {
		t.Fatalf("read = %q, %v", got, err)
	}
}

func TestObjectStoreRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	store, err := NewObjectStore(root)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.RemoveAll(filepath.Join(root, "objects")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "objects")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if _, _, err := store.Create("/safe.txt"); err == nil {
		t.Fatal("Create accepted objects symlink escape")
	}
	if _, err := store.Open("objects/aa/" + strings.Repeat("a", 64)); err == nil {
		t.Fatal("Open accepted symlink escape")
	}
}
