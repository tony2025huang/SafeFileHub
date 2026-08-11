// Package storage maps logical files to opaque objects below a configured root.
package storage

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ObjectStore stores completed objects under an opaque, randomly generated key.
type ObjectStore struct{ root string }

// NewObjectStore resolves the configured physical root once. Object paths are
// always derived from generated keys, never from logical user paths.
func NewObjectStore(root string) (*ObjectStore, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve storage root: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("stat storage root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("storage root is not a directory")
	}
	return &ObjectStore{root: resolved}, nil
}

// Create creates a new completed object. logicalPath is deliberately not used
// in choosing its physical name; its validation belongs to the request layer.
func (s *ObjectStore) Create(logicalPath string) (string, io.WriteCloser, error) {
	_ = logicalPath
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", nil, fmt.Errorf("generate object key: %w", err)
	}
	name := hex.EncodeToString(random[:])
	key := "objects/" + name[:2] + "/" + name
	path, err := s.objectPath(key, true)
	if err != nil {
		return "", nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", nil, fmt.Errorf("create object: %w", err)
	}
	return key, file, nil
}

// Open opens only a validated opaque object key below the physical root.
func (s *ObjectStore) Open(key string) (*os.File, error) {
	path, err := s.objectPath(key, false)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat object: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("object is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open object: %w", err)
	}
	return file, nil
}

func (s *ObjectStore) objectPath(key string, create bool) (string, error) {
	parts := strings.Split(key, "/")
	if len(parts) != 3 || parts[0] != "objects" || len(parts[1]) != 2 || len(parts[2]) != 64 || parts[1] != parts[2][:2] || !isLowerHex(parts[1]) || !isLowerHex(parts[2]) {
		return "", fmt.Errorf("invalid object key")
	}
	objects := filepath.Join(s.root, "objects")
	shard := filepath.Join(objects, parts[1])
	if create {
		if err := mkdirSafe(s.root, objects); err != nil {
			return "", err
		}
		if err := mkdirSafe(objects, shard); err != nil {
			return "", err
		}
	} else if err := safeDirectory(objects); err != nil {
		return "", err
	} else if err := safeDirectory(shard); err != nil {
		return "", err
	}
	return filepath.Join(shard, parts[2]), nil
}

func mkdirSafe(parent, directory string) error {
	if err := safeDirectory(parent); err != nil {
		return err
	}
	if err := os.Mkdir(directory, 0700); err != nil && !os.IsExist(err) {
		return fmt.Errorf("create object directory: %w", err)
	}
	return safeDirectory(directory)
}
func safeDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect object directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("object directory is unsafe")
	}
	return nil
}
func isLowerHex(value string) bool {
	for _, b := range []byte(value) {
		if !(b >= '0' && b <= '9' || b >= 'a' && b <= 'f') {
			return false
		}
	}
	return true
}
