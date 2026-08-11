package storage

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func newTestObjectStore(t *testing.T, root string) *ObjectStore {
	t.Helper()
	store, err := NewObjectStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close object store: %v", err)
		}
	})
	return store
}

func TestObjectStoreCloseIsIdempotent(t *testing.T) {
	store := newTestObjectStore(t, t.TempDir())
	if err := store.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, _, err := store.Create("ignored"); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Create after Close error = %v, want os.ErrClosed", err)
	}
	if _, err := store.Open("objects/aa/" + strings.Repeat("a", 64)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Open after Close error = %v, want os.ErrClosed", err)
	}
}

func TestObjectCreationUsesRandomKeysRatherThanLogicalPaths(t *testing.T) {
	store := newTestObjectStore(t, t.TempDir())

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
	store := newTestObjectStore(t, root)
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
	store := newTestObjectStore(t, root)
	outside := t.TempDir()
	objects := filepath.Join(root, "objects")
	if err := os.Mkdir(objects, 0700); err != nil {
		t.Fatal(err)
	}
	backup := objects + ".backup"

	stop := make(chan struct{})
	started := make(chan struct{})
	done := make(chan struct{})
	var replacements, creates, createErrors atomic.Int64
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := os.Rename(objects, backup); err == nil {
				if err := os.Symlink(outside, objects); err == nil {
					replacements.Add(1)
					select {
					case <-started:
					default:
						close(started)
					}
					_ = os.Remove(objects) // Remove may race only with this goroutine's shutdown.
				}
				_ = os.Rename(backup, objects)
			}
		}
	}()
	defer func() { close(stop); <-done }()
	<-started // Do not start object operations until the symlink attack window has opened.

	for range 500 {
		key, writer, err := store.Create("ignored")
		if err == nil {
			creates.Add(1)
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if matches, globErr := filepath.Glob(filepath.Join(outside, "*", filepath.Base(key))); globErr != nil || len(matches) != 0 {
				t.Fatalf("Create escaped storage root through objects symlink: %q (matches=%v, err=%v)", key, matches, globErr)
			}
		} else {
			createErrors.Add(1)
		}
	}
	if replacements.Load() == 0 || creates.Load() == 0 {
		t.Fatalf("attack coverage incomplete: replacements=%d creates=%d createErrors=%d", replacements.Load(), creates.Load(), createErrors.Load())
	}
}

// Open must not turn a regular-file check into a symlink-following open when
// the object is replaced between those operations.
func TestOpenDoesNotFollowConcurrentShardSymlinkReplacement(t *testing.T) {
	root := t.TempDir()
	store := newTestObjectStore(t, root)
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
	started := make(chan struct{})
	done := make(chan struct{})
	var replacements, opens, openErrors atomic.Int64
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := os.Rename(shard, backup); err == nil {
				if err := os.Symlink(outside, shard); err == nil {
					replacements.Add(1)
					select {
					case <-started:
					default:
						close(started)
					}
					_ = os.Remove(shard)
				}
				_ = os.Rename(backup, shard)
			}
		}
	}()
	defer func() { close(stop); <-done }()
	<-started // Ensure Open runs while the shard is actively replaced.

	for range 500 {
		reader, err := store.Open(key)
		if err == nil {
			opens.Add(1)
			contents, readErr := io.ReadAll(reader)
			closeErr := reader.Close()
			if readErr != nil || closeErr != nil {
				t.Fatalf("read object: %v, close: %v", readErr, closeErr)
			}
			if string(contents) == "outside" {
				t.Fatal("Open followed a replacement shard symlink outside storage root")
			}
		} else {
			openErrors.Add(1)
		}
	}
	if replacements.Load() == 0 || opens.Load() == 0 {
		t.Fatalf("attack coverage incomplete: replacements=%d opens=%d openErrors=%d", replacements.Load(), opens.Load(), openErrors.Load())
	}
}

func TestOpenDoesNotFollowConcurrentObjectSymlinkReplacement(t *testing.T) {
	root := t.TempDir()
	store := newTestObjectStore(t, root)
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
