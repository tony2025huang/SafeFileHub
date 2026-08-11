package limits

import (
	"errors"
	"testing"
)

func TestLimitGlobalPerUserAndPerIPLeases(t *testing.T) {
	limiter, err := NewUploadLimiter(2, 1, 1)
	if err != nil {
		t.Fatal(err)
	}

	first, err := limiter.TryAcquire("alice", "192.0.2.1")
	if err != nil {
		t.Fatalf("first lease: %v", err)
	}
	defer first.Release()

	for _, key := range []UploadKey{{User: "alice", IP: "192.0.2.2"}, {User: "bob", IP: "192.0.2.1"}} {
		if _, err := limiter.TryAcquire(key.User, key.IP); !errors.Is(err, ErrLimited) {
			t.Errorf("TryAcquire(%q, %q) error = %v, want ErrLimited", key.User, key.IP, err)
		}
	}

	second, err := limiter.TryAcquire("bob", "192.0.2.2")
	if err != nil {
		t.Fatalf("second lease: %v", err)
	}
	defer second.Release()
	if _, err := limiter.TryAcquire("carol", "192.0.2.3"); !errors.Is(err, ErrLimited) {
		t.Fatalf("global cap error = %v, want ErrLimited", err)
	}
}

func TestLeaseReleaseIsIdempotentAndMakesCapacityAvailable(t *testing.T) {
	limiter, err := NewUploadLimiter(1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := limiter.TryAcquire("alice", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	lease.Release()
	if _, err := limiter.TryAcquire("alice", "192.0.2.1"); err != nil {
		t.Fatalf("capacity was not released: %v", err)
	}
}

func TestLimitRejectsInvalidCaps(t *testing.T) {
	for _, caps := range [][3]int{{0, 1, 1}, {1, 0, 1}, {1, 1, 0}} {
		if _, err := NewUploadLimiter(caps[0], caps[1], caps[2]); err == nil {
			t.Fatalf("NewUploadLimiter(%v) succeeded", caps)
		}
	}
}
