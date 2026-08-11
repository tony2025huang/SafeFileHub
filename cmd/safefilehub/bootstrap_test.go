package main

import (
	"context"
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
	u, e := repo.UserByID(context.Background(), 1)
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
	u, _ := repo.UserByID(context.Background(), 1)
	if u.Disabled || !auth.VerifyPassword(u.PasswordHash, next.Password) {
		t.Fatal("reset credentials bad")
	}
}
