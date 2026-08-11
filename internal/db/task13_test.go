package db_test

import (
	"context"
	"github.com/example/safefilehub/internal/db"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditVisibilityIsRedacted(t *testing.T) {
	repo, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "audit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ctx := context.Background()
	u, err := repo.CreateUser(ctx, db.User{Username: "auditor", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.CreateAuditEvent(ctx, db.AuditEvent{UserID: u.ID, Action: "upload.create", LogicalPath: "/safe.txt", Detail: "password=hunter2 content=secret status=201"})
	if err != nil {
		t.Fatal(err)
	}
	events, err := repo.AuditEventsForUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Action != "upload.create" || events[0].LogicalPath != "/safe.txt" || events[0].Status != 201 {
		t.Fatalf("events=%#v", events)
	}
	if strings.Contains(events[0].Detail, "hunter2") || strings.Contains(events[0].Detail, "secret") {
		t.Fatalf("secret detail leaked: %q", events[0].Detail)
	}
}
