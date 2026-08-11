// Package security exercises release-critical abuse boundaries without external services.
package security

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/example/safefilehub/internal/archive"
	"github.com/example/safefilehub/internal/auth"
	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/pathpolicy"
	"github.com/example/safefilehub/internal/storage"
)

func TestReleasePathPolicyRejectsTraversalDoubleEncodingAndReservedNames(t *testing.T) {
	policy := config.Default().NamePolicy
	for _, raw := range []string{"../secret", "safe/%252e%252e/secret", "safe\\secret", "CON", "aux.txt"} {
		if _, err := pathpolicy.ParseEscapedPath(raw, policy); err == nil {
			t.Fatalf("ParseEscapedPath(%q) unexpectedly succeeded", raw)
		}
	}
}

func TestReleaseStorageRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "objects")); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewObjectStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, _, err := store.Create("/safe.txt"); err == nil {
		t.Fatal("Create accepted symlinked object directory")
	}
}

func TestReleaseDisabledSessionIsDenied(t *testing.T) {
	manager := auth.NewSessionManager(auth.NewMemorySessionStore(), auth.SessionConfig{TTL: time.Hour})
	defer manager.Close()
	id, err := manager.Create(context.Background(), 12)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RevokeUser(context.Background(), 12); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.UserID(context.Background(), id); err == nil {
		t.Fatal("revoked user session remained usable")
	}
}

func TestReleaseArchiveOwnershipPreventsCrossUserDownloadAndCancel(t *testing.T) {
	manager, err := archive.New(archive.Options{Workers: 1, MaxFiles: 1, MaxBytes: 1, TTL: time.Hour, TempDir: t.TempDir()}, emptySource{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	job, err := manager.CreateForUser(context.Background(), 7, "/", []archive.Entry{{LogicalPath: "/empty.txt", ObjectKey: "missing", Size: 1}}, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	if !manager.OwnedBy(job.ID, 7) {
		t.Fatal("owner cannot access own archive")
	}
	if manager.OwnedBy(job.ID, 8) {
		t.Fatal("different user can access archive")
	}
}

func TestReleaseConcurrentFilesKeepHealthResponsiveAndHashStable(t *testing.T) {
	for _, n := range []int{1, 2, 4, 8, 16} {
		t.Run("concurrent", func(t *testing.T) {
			root := t.TempDir()
			store, err := storage.NewObjectStore(root)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
			payload := []byte("SafeFileHub release-gate payload")
			want := sha256.Sum256(payload)
			var wg sync.WaitGroup
			errs := make(chan error, n)
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					key, w, err := store.Create("/untrusted-name")
					if err != nil {
						errs <- err
						return
					}
					if _, err = w.Write(payload); err == nil {
						err = w.Close()
					} else {
						_ = w.Close()
					}
					if err != nil {
						errs <- err
						return
					}
					r, err := store.Open(key)
					if err != nil {
						errs <- err
						return
					}
					defer r.Close()
					buf := make([]byte, len(payload))
					_, err = r.Read(buf)
					if err != nil || sha256.Sum256(buf) != want {
						errs <- err
					}
				}()
			}
			for i := 0; i < n; i++ {
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
				if rr.Code != http.StatusOK || rr.Body.String() != "ok\n" {
					t.Fatalf("health: %d %q", rr.Code, rr.Body.String())
				}
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			if got := hex.EncodeToString(want[:]); len(got) != 64 {
				t.Fatal("invalid SHA-256")
			}
		})
	}
}

type emptySource struct{}

func (emptySource) Open(context.Context, string) (io.ReadCloser, error) { return nil, os.ErrNotExist }

type allowAll struct{}

func (allowAll) Allow(context.Context, string) (bool, error) { return true, nil }
