//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

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
	"sync"

	"golang.org/x/sys/unix"
)

// ObjectStore stores completed objects under an opaque, randomly generated key.
// root is retained only for diagnostics; all object operations are rooted at fd.
type ObjectStore struct {
	root string
	mu   sync.RWMutex
	fd   int
}

// NewObjectStore resolves the configured physical root once and retains an open
// directory descriptor. Subsequent operations are relative to that descriptor,
// so replacing the root path cannot redirect object operations.
func NewObjectStore(root string) (*ObjectStore, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve storage root: %w", err)
	}
	fd, err := unix.Open(resolved, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open storage root: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("stat storage root: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("storage root is not a directory")
	}
	return &ObjectStore{root: resolved, fd: fd}, nil
}

// Close releases the root directory descriptor. It is safe to call repeatedly.
func (s *ObjectStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fd < 0 {
		return nil
	}
	fd := s.fd
	s.fd = -1
	if err := unix.Close(fd); err != nil {
		return fmt.Errorf("close storage root: %w", err)
	}
	return nil
}

func (s *ObjectStore) rootFD() (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.fd < 0 {
		return -1, os.ErrClosed
	}
	return unix.Dup(s.fd)
}

// Create creates a new completed object. logicalPath is deliberately not used
// in choosing its physical name; its validation belongs to the request layer.
func (s *ObjectStore) Create(logicalPath string) (string, io.WriteCloser, error) {
	_ = logicalPath
	rootFD, err := s.rootFD()
	if err != nil {
		return "", nil, err
	}
	defer unix.Close(rootFD)
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", nil, fmt.Errorf("generate object key: %w", err)
	}
	name := hex.EncodeToString(random[:])
	key := "objects/" + name[:2] + "/" + name
	objects, shard, err := objectDirectory(rootFD, key, true)
	if err != nil {
		return "", nil, err
	}
	defer unix.Close(objects)
	defer unix.Close(shard)

	fd, err := unix.Openat(shard, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return "", nil, fmt.Errorf("create object: %w", err)
	}
	return key, os.NewFile(uintptr(fd), key), nil
}

// Open opens only a validated opaque object key below the physical root.
func (s *ObjectStore) Open(key string) (*os.File, error) {
	rootFD, err := s.rootFD()
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)
	_, name, err := parseObjectKey(key)
	if err != nil {
		return nil, err
	}
	objects, shard, err := objectDirectory(rootFD, key, false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(objects)
	defer unix.Close(shard)

	fd, err := unix.Openat(shard, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open object: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("stat object: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("object is not a regular file")
	}
	return os.NewFile(uintptr(fd), key), nil
}

// objectDirectory opens objects and its shard relative to the already-open
// storage root. Each component uses O_NOFOLLOW, preventing a rename/symlink
// race from escaping the root after validation.
func objectDirectory(rootFD int, key string, create bool) (objects, shard int, err error) {
	prefix, name, err := parseObjectKey(key)
	if err != nil {
		return -1, -1, err
	}
	objects, err = openDirectoryAt(rootFD, "objects", create)
	if err != nil {
		return -1, -1, err
	}
	shard, err = openDirectoryAt(objects, prefix, create)
	if err != nil {
		unix.Close(objects)
		return -1, -1, err
	}
	_ = name
	return objects, shard, nil
}

func openDirectoryAt(parent int, name string, create bool) (int, error) {
	if create {
		if err := unix.Mkdirat(parent, name, 0700); err != nil && err != unix.EEXIST {
			return -1, fmt.Errorf("create object directory: %w", err)
		}
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open object directory: %w", err)
	}
	return fd, nil
}

func parseObjectKey(key string) (prefix, name string, err error) {
	parts := strings.Split(key, "/")
	if len(parts) != 3 || parts[0] != "objects" || len(parts[1]) != 2 || len(parts[2]) != 64 || parts[1] != parts[2][:2] || !isLowerHex(parts[1]) || !isLowerHex(parts[2]) {
		return "", "", fmt.Errorf("invalid object key")
	}
	return parts[1], parts[2], nil
}

func isLowerHex(value string) bool {
	for _, b := range []byte(value) {
		if !(b >= '0' && b <= '9') && !(b >= 'a' && b <= 'f') {
			return false
		}
	}
	return true
}

// CreateStaging creates a private resumable-upload staging file below the
// already-open store root. Its relative name is caller-generated and never
// returned by HTTP handlers.
func (s *ObjectStore) CreateStaging(name string) (*os.File, error) {
	if !validStagingName(name) {
		return nil, fmt.Errorf("invalid staging name")
	}
	rootFD, err := s.rootFD()
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)
	d, err := openDirectoryAt(rootFD, "staging", true)
	if err != nil {
		return nil, err
	}
	defer unix.Close(d)
	fd, err := unix.Openat(d, strings.TrimPrefix(name, "staging/"), unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, fmt.Errorf("create staging: %w", err)
	}
	return os.NewFile(uintptr(fd), name), nil
}
func (s *ObjectStore) OpenStaging(name string) (*os.File, error) {
	return s.openStaging(name, unix.O_RDONLY)
}
func (s *ObjectStore) OpenStagingWrite(name string) (*os.File, error) {
	return s.openStaging(name, unix.O_WRONLY)
}
func (s *ObjectStore) openStaging(name string, flags int) (*os.File, error) {
	if !validStagingName(name) {
		return nil, fmt.Errorf("invalid staging name")
	}
	rootFD, err := s.rootFD()
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)
	d, err := openDirectoryAt(rootFD, "staging", false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(d)
	fd, err := unix.Openat(d, strings.TrimPrefix(name, "staging/"), flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open staging: %w", err)
	}
	return os.NewFile(uintptr(fd), name), nil
}
func (s *ObjectStore) RemoveStaging(name string) error {
	if !validStagingName(name) {
		return fmt.Errorf("invalid staging name")
	}
	rootFD, err := s.rootFD()
	if err != nil {
		return err
	}
	defer unix.Close(rootFD)
	d, err := openDirectoryAt(rootFD, "staging", false)
	if err != nil {
		return err
	}
	defer unix.Close(d)
	if err := unix.Unlinkat(d, strings.TrimPrefix(name, "staging/"), 0); err != nil && err != unix.ENOENT {
		return fmt.Errorf("remove staging: %w", err)
	}
	return nil
}
func validStagingName(name string) bool {
	return strings.HasPrefix(name, "staging/") && len(strings.TrimPrefix(name, "staging/")) == 37 && strings.HasSuffix(name, ".part") && !strings.Contains(strings.TrimPrefix(name, "staging/"), "/")
}
