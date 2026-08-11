//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package storage

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func TestObjectStoreCloseClosesRootDescriptor(t *testing.T) {
	store := newTestObjectStore(t, t.TempDir())
	fd := store.fd
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("root descriptor %d remains open: %v", fd, err)
	}
}
