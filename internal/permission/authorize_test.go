package permission_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/permission"
)

func TestAuthorizeUsesLongestPrefixAndExplicitDenyWins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := testRepository(t)
	user := createUser(t, ctx, repo)
	root := createRoot(t, ctx, repo)
	for _, p := range []db.Permission{
		{UserID: user.ID, RootID: root.ID, PathPrefix: "/", Action: "read", Allow: true},
		{UserID: user.ID, RootID: root.ID, PathPrefix: "/projects", Action: "read", Allow: false},
		{UserID: user.ID, RootID: root.ID, PathPrefix: "/projects/public", Action: "read", Allow: true},
		// A deny at the same most-specific prefix must win regardless of insertion order.
		{UserID: user.ID, RootID: root.ID, PathPrefix: "/projects/public", Action: "read", Allow: false},
	} {
		if _, err := repo.CreatePermission(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	authorizer := permission.NewAuthorizer(repo, config.Default().NamePolicy)
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/projects/private/notes.txt", false}, {"/projects/public/guide.txt", false}, {"/other/readme.txt", true},
	} {
		if got, err := authorizer.Authorize(ctx, user.ID, root.ID, tc.path, "read"); err != nil || got != tc.want {
			t.Errorf("Authorize(%q) = %v, %v; want %v, nil", tc.path, got, err, tc.want)
		}
	}
}

func TestAuthorizeDefaultsDenyAndActionsDoNotImplyEachOther(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := testRepository(t)
	user := createUser(t, ctx, repo)
	root := createRoot(t, ctx, repo)
	if _, err := repo.CreatePermission(ctx, db.Permission{UserID: user.ID, RootID: root.ID, PathPrefix: "/docs", Action: "read", Allow: true}); err != nil {
		t.Fatal(err)
	}
	authorizer := permission.NewAuthorizer(repo, config.Default().NamePolicy)
	for _, action := range []string{"write", "delete", "archive"} {
		if got, err := authorizer.Authorize(ctx, user.ID, root.ID, "/docs/a.txt", action); err != nil || got {
			t.Errorf("read implied %s: got %v, %v; want false, nil", action, got, err)
		}
	}
	if got, err := authorizer.Authorize(ctx, user.ID, root.ID, "/ungranted/a.txt", "read"); err != nil || got {
		t.Errorf("default decision = %v, %v; want false, nil", got, err)
	}
}

func TestAuthorizeRejectsNonCanonicalLogicalPaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := testRepository(t)
	authorizer := permission.NewAuthorizer(repo, config.Default().NamePolicy)
	if _, err := authorizer.Authorize(ctx, 1, 1, "/docs/../secret", "read"); err == nil {
		t.Fatal("unsafe path was authorized")
	}
	if _, err := authorizer.Authorize(ctx, 1, 1, "docs/a.txt", "read"); err == nil {
		t.Fatal("non-canonical input was authorized")
	}
}

func testRepository(t *testing.T) *db.Repository {
	t.Helper()
	repo, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "permission.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}
func createUser(t *testing.T, ctx context.Context, repo *db.Repository) db.User {
	t.Helper()
	got, err := repo.CreateUser(ctx, db.User{Username: t.Name(), PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	return got
}
func createRoot(t *testing.T, ctx context.Context, repo *db.Repository) db.StorageRoot {
	t.Helper()
	got, err := repo.CreateStorageRoot(ctx, db.StorageRoot{Name: t.Name(), Path: "/srv/" + t.Name()})
	if err != nil {
		t.Fatal(err)
	}
	return got
}
