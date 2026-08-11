//go:build windows

// Package storage maps logical files to opaque objects below a configured root.
package storage

import (
	"crypto/sha256"
	"errors"
	"io"
	"os"
)

// ErrUnsupportedPlatform reports that the secure descriptor-relative object
// store is unavailable on this platform. Windows is deliberately unsupported
// until an equivalent race-safe implementation is provided; operations never
// silently fall back to path-based traversal.
var ErrUnsupportedPlatform = errors.New("safe object storage is unsupported on windows")

// ObjectStore is unavailable on Windows; see ErrUnsupportedPlatform.
type ObjectStore struct{}

// NewObjectStore always returns ErrUnsupportedPlatform on Windows.
func NewObjectStore(root string) (*ObjectStore, error) {
	return nil, ErrUnsupportedPlatform
}

// Close is a no-op for a nil or unsupported Windows ObjectStore.
func (s *ObjectStore) Close() error { return nil }

// Create always returns ErrUnsupportedPlatform on Windows.
func (s *ObjectStore) Create(logicalPath string) (string, io.WriteCloser, error) {
	return "", nil, ErrUnsupportedPlatform
}

// Open always returns ErrUnsupportedPlatform on Windows.
func (s *ObjectStore) Open(key string) (*os.File, error) {
	return nil, ErrUnsupportedPlatform
}

func (s *ObjectStore) CreateStaging(name string) (*os.File, error) {
	return nil, ErrUnsupportedPlatform
}
func (s *ObjectStore) OpenStaging(name string) (*os.File, error) { return nil, ErrUnsupportedPlatform }
func (s *ObjectStore) OpenStagingWrite(name string) (*os.File, error) {
	return nil, ErrUnsupportedPlatform
}
func (s *ObjectStore) LockStagingLifecycle(name string) (*os.File, error) {
	return nil, ErrUnsupportedPlatform
}
func (s *ObjectStore) UnlockStagingLifecycle(f *os.File) error { return nil }
func (s *ObjectStore) RemoveStaging(name string) error         { return ErrUnsupportedPlatform }

func (s *ObjectStore) HashAndSyncStaging(name string) (int64, [sha256.Size]byte, error) {
	return 0, [sha256.Size]byte{}, ErrUnsupportedPlatform
}
func (s *ObjectStore) PublishStaging(name string) (string, error)     { return "", ErrUnsupportedPlatform }
func (s *ObjectStore) StagingNames(limit int) ([]string, error)       { return nil, ErrUnsupportedPlatform }
func (s *ObjectStore) RestorePublished(key, stagingName string) error { return ErrUnsupportedPlatform }
