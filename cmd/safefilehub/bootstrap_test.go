package main

import (
	"context"
	"encoding/base64"
	"github.com/example/safefilehub/internal/auth"
	"github.com/example/safefilehub/internal/db"
	"strings"
	"testing"
)

func TestBootstrapCreatesRandomLoginOnlyOnceAndStoresHash(t *testing.T) {
	cfg := testConfig(t)
	repo, e := db.Open(context.Background(), cfg.SQLitePath)
	if e != nil {
		t.Fatal(e)
	}
	defer repo.Close()
	first, e := bootstrapInitialAdmin(context.Background(), repo)
	if e != nil {
		t.Fatal(e)
	}
	if first.Password == "" || first.Username == "" || strings.Contains(first.Username, "admin") {
		t.Fatalf("unsafe bootstrap %#v", first)
	}
	u, e := repo.BootstrapAdmin(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if u.PasswordHash == first.Password || !auth.VerifyPassword(u.PasswordHash, first.Password) {
		t.Fatal("password not safely stored")
	}
	second, e := bootstrapInitialAdmin(context.Background(), repo)
	if e != nil || second.Created {
		t.Fatalf("rebootstrap %#v %v", second, e)
	}
}
func TestResetInitialAdminFailsWhenMissingAndResetsExisting(t *testing.T) {
	cfg := testConfig(t)
	repo, e := db.Open(context.Background(), cfg.SQLitePath)
	if e != nil {
		t.Fatal(e)
	}
	defer repo.Close()
	if _, e := resetInitialAdmin(context.Background(), repo); e == nil {
		t.Fatal("missing admin reset succeeded")
	}
	first, e := bootstrapInitialAdmin(context.Background(), repo)
	if e != nil {
		t.Fatal(e)
	}
	next, e := resetInitialAdmin(context.Background(), repo)
	if e != nil || next.Password == first.Password {
		t.Fatalf("reset %#v %v", next, e)
	}
	u, _ := repo.BootstrapAdmin(context.Background())
	if u.Disabled || !auth.VerifyPassword(u.PasswordHash, next.Password) {
		t.Fatal("reset credentials bad")
	}
}

func TestBootstrapDoesNotPromoteExistingFirstUser(t *testing.T) {
	cfg := testConfig(t)
	repo, err := db.Open(context.Background(), cfg.SQLitePath)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ordinary, err := repo.CreateUser(context.Background(), db.User{Username: "ordinary", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if ordinary.ID != 1 {
		t.Fatalf("ordinary user ID = %d, want 1", ordinary.ID)
	}
	created, err := bootstrapInitialAdmin(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if !created.Created {
		t.Fatal("bootstrap admin was not created")
	}
	if bootstrap, err := repo.IsBootstrapAdmin(context.Background(), ordinary.ID); err != nil || bootstrap {
		t.Fatalf("ordinary first user bootstrap=%t err=%v", bootstrap, err)
	}
	bootstrap, err := repo.BootstrapAdmin(context.Background())
	if err != nil || bootstrap.ID == ordinary.ID || bootstrap.Username != created.Username {
		t.Fatalf("bootstrap=%#v err=%v", bootstrap, err)
	}
}

func TestBootstrapAdoptsOnlyLegacyGeneratedIdentity(t *testing.T) {
	cfg := testConfig(t)
	repo, err := db.Open(context.Background(), cfg.SQLitePath)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	legacyName := "sfh-" + base64.RawURLEncoding.EncodeToString(make([]byte, 24))
	legacy, err := repo.CreateUser(context.Background(), db.User{Username: legacyName, PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrapInitialAdmin(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := repo.BootstrapAdmin(context.Background())
	if err != nil || bootstrap.ID != legacy.ID {
		t.Fatalf("legacy bootstrap=%#v err=%v", bootstrap, err)
	}
}
