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
		t.Fatalf("create test symlink: %v", err)
	}
	if _, _, err := store.Create("/safe.txt"); err == nil {
		t.Fatal("Create accepted objects symlink escape")
	}
	if _, err := store.Open("objects/aa/" + strings.Repeat("a", 64)); err == nil {
		t.Fatal("Open accepted symlink escape")
	}
}

// This test repeatedly swaps an already-validated path component for a
// symlink. Create must use directory FDs, not validate then reopen by path.
func TestCreateDoesNotFollowConcurrentShardSymlinkReplacement(t *testing.T) {
	root := t.TempDir()
	store, err := NewObjectStore(root)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	objects := filepath.Join(root, "objects")
	if err := os.Mkdir(objects, 0700); err != nil {
		t.Fatal(err)
	}
	backup := objects + ".backup"

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Rename(objects, backup)
			_ = os.Symlink(outside, objects)
			_ = os.Remove(objects)
			_ = os.Rename(backup, objects)
		}
	}()
	defer func() { close(stop); <-done }()

	for range 500 {
		key, writer, err := store.Create("ignored")
		if err == nil {
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if matches, globErr := filepath.Glob(filepath.Join(outside, "*", filepath.Base(key))); globErr != nil || len(matches) != 0 {
				t.Fatalf("Create escaped storage root through objects symlink: %q (matches=%v, err=%v)", key, matches, globErr)
			}
		}
	}
}

// Open must not turn a regular-file check into a symlink-following open when
// the object is replaced between those operations.
func TestOpenDoesNotFollowConcurrentShardSymlinkReplacement(t *testing.T) {
	root := t.TempDir()
	store, err := NewObjectStore(root)
	if err != nil {
		t.Fatal(err)
	}
	key, writer, err := store.Create("ignored")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, "inside"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, filepath.Base(key)), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	shard := filepath.Dir(filepath.Join(root, filepath.FromSlash(key)))
	backup := shard + ".backup"

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Rename(shard, backup)
			_ = os.Symlink(outside, shard)
			_ = os.Remove(shard)
			_ = os.Rename(backup, shard)
		}
	}()
	defer func() { close(stop); <-done }()

	for range 500 {
		reader, err := store.Open(key)
		if err == nil {
			contents, readErr := io.ReadAll(reader)
			closeErr := reader.Close()
			if readErr != nil || closeErr != nil {
				t.Fatalf("read object: %v, close: %v", readErr, closeErr)
			}
			if string(contents) == "outside" {
				t.Fatal("Open followed a replacement shard symlink outside storage root")
			}
		}
	}
}

func TestOpenDoesNotFollowConcurrentObjectSymlinkReplacement(t *testing.T) {
	root := t.TempDir()
	store, err := NewObjectStore(root)
	if err != nil {
		t.Fatal(err)
	}
	key, writer, err := store.Create("ignored")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, "inside"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, filepath.FromSlash(key))
	backup := path + ".backup"

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Rename(path, backup)
			_ = os.Symlink(outside, path)
			_ = os.Remove(path)
			_ = os.Rename(backup, path)
		}
	}()
	defer func() { close(stop); <-done }()

	for range 500 {
		reader, err := store.Open(key)
		if err == nil {
			contents, readErr := io.ReadAll(reader)
			closeErr := reader.Close()
			if readErr != nil || closeErr != nil {
				t.Fatalf("read object: %v, close: %v", readErr, closeErr)
			}
			if string(contents) == "outside" {
				t.Fatal("Open followed a replacement symlink outside storage root")
			}
		}
	}
}
