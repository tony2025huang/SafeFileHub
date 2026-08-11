package db_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/example/safefilehub/internal/db"
)

func TestSiteSettingsAssetsAndValidation(t *testing.T) {
	ctx := context.Background()
	repo, err := db.Open(ctx, filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	settings, err := repo.SiteSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.SiteName != "SafeFileHub" || settings.PrimaryColor != "#2563EB" || settings.FilingEnabled || settings.MD5Enabled {
		t.Fatalf("defaults = %#v", settings)
	}
	settings.SiteName, settings.PrimaryColor, settings.FilingEnabled, settings.FilingText, settings.MD5Enabled = "Files", "#abcdef", true, "ICP 123", true
	if err := repo.UpdateSiteSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateSiteSettings(ctx, db.SiteSettings{SiteName: "", PrimaryColor: "red"}); err == nil {
		t.Fatal("invalid settings accepted")
	}

	asset, err := repo.ReplaceSiteAsset(ctx, "login_logo", db.SiteAsset{OpaqueKey: "opaque-1", ContentType: "image/png", Size: 10, Width: 1, Height: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PublicSiteAssetByID(ctx, asset.ID); err != nil {
		t.Fatal(err)
	}
	asset2, err := repo.ReplaceSiteAsset(ctx, "login_logo", db.SiteAsset{OpaqueKey: "opaque-2", ContentType: "image/png", Size: 11, Width: 2, Height: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PublicSiteAssetByID(ctx, asset.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("replaced asset = %v, want absent", err)
	}
	if err := repo.DeleteSiteAsset(ctx, asset2.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteSiteAsset(ctx, asset2.ID); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	settings, err = repo.SiteSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.LoginLogoAssetID != 0 {
		t.Fatalf("deleted asset remains configured: %#v", settings)
	}
}

func TestMD5TasksFollowCompletionSettingAndLifecycle(t *testing.T) {
	ctx := context.Background()
	repo, err := db.Open(ctx, filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	u, err := repo.CreateUser(ctx, db.User{Username: "u", PasswordHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.CreateStorageRoot(ctx, db.StorageRoot{Name: "r", Path: "/r"})
	if err != nil {
		t.Fatal(err)
	}
	complete := func(id, path string) {
		t.Helper()
		s, e := repo.CreateUploadSession(ctx, db.UploadSession{ID: id, UserID: u.ID, RootID: r.ID, LogicalPath: path, StagingPath: id, Length: 1, ExpiresAt: time.Now().Add(time.Hour)})
		if e != nil {
			t.Fatal(e)
		}
		if e = repo.CompleteUpload(ctx, db.File{RootID: r.ID, LogicalPath: path, ObjectKey: "obj-" + id, Size: 1, CreatedByUserID: u.ID}, s.ID); e != nil {
			t.Fatal(e)
		}
	}
	complete("off", "/off")
	old, _ := repo.FileByRootAndPath(ctx, r.ID, "/off")
	if old.MD5Status != db.MD5Disabled {
		t.Fatalf("off status = %q", old.MD5Status)
	}
	settings, _ := repo.SiteSettings(ctx)
	settings.MD5Enabled = true
	if err := repo.UpdateSiteSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	complete("on", "/on")
	file, _ := repo.FileByRootAndPath(ctx, r.ID, "/on")
	if file.MD5Status != db.MD5Pending {
		t.Fatalf("on status = %q", file.MD5Status)
	}
	task, err := repo.ClaimMD5Task(ctx)
	if err != nil || task.FileID != file.ID || task.Status != db.MD5Computing {
		t.Fatalf("claim = %#v, %v", task, err)
	}
	if err := repo.FailMD5Task(ctx, task.FileID, "temporary"); err != nil {
		t.Fatal(err)
	}
	task, err = repo.ClaimMD5Task(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteMD5Task(ctx, task.FileID, "d41d8cd98f00b204e9800998ecf8427e"); err != nil {
		t.Fatal(err)
	}
	file, _ = repo.FileByID(ctx, file.ID)
	if file.MD5Status != db.MD5Ready || file.MD5Digest == "" {
		t.Fatalf("completed file = %#v", file)
	}
}

func TestMD5ClaimIsUniqueAcrossConnectionsAndRecoveryBounded(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "metadata.sqlite")
	one, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	two, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	u, _ := one.CreateUser(ctx, db.User{Username: "u", PasswordHash: "h"})
	r, _ := one.CreateStorageRoot(ctx, db.StorageRoot{Name: "r", Path: "/r"})
	s, _ := one.SiteSettings(ctx)
	s.MD5Enabled = true
	if err := one.UpdateSiteSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		id := string(rune('a' + i))
		_, err := one.CreateUploadSession(ctx, db.UploadSession{ID: id, UserID: u.ID, RootID: r.ID, LogicalPath: "/" + id, StagingPath: id, Length: 1, ExpiresAt: time.Now().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		if err = one.CompleteUpload(ctx, db.File{RootID: r.ID, LogicalPath: "/" + id, ObjectKey: id, Size: 1, CreatedByUserID: u.ID}, id); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	got := make(chan int64, 2)
	for _, repo := range []*db.Repository{one, two} {
		wg.Add(1)
		go func(repo *db.Repository) {
			defer wg.Done()
			task, e := repo.ClaimMD5Task(ctx)
			if e == nil {
				got <- task.FileID
			}
		}(repo)
	}
	wg.Wait()
	close(got)
	seen := map[int64]bool{}
	for id := range got {
		if seen[id] {
			t.Fatal("same task claimed twice")
		}
		seen[id] = true
	}
	if len(seen) != 2 {
		t.Fatalf("claims = %d, want 2", len(seen))
	}
	if n, err := one.RequeueComputingMD5Tasks(ctx, 1); err != nil || n != 1 {
		t.Fatalf("requeue = %d, %v", n, err)
	}
}

func TestSiteAssetReplacementQueuesOnlyUnreferencedOldKey(t *testing.T) {
	ctx := context.Background()
	repo, err := db.Open(ctx, filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	one, err := repo.ReplaceSiteAsset(ctx, "favicon", db.SiteAsset{OpaqueKey: "site/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.png", ContentType: "image/png", Size: 1, Width: 1, Height: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReplaceSiteAsset(ctx, "favicon", db.SiteAsset{OpaqueKey: "site/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.png", ContentType: "image/png", Size: 1, Width: 1, Height: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PublicSiteAssetByID(ctx, one.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("old metadata remains: %v", err)
	}
	keys, err := repo.SiteAssetCleanupKeys(ctx, 10)
	if err != nil || len(keys) != 1 || keys[0] != one.OpaqueKey {
		t.Fatalf("cleanup keys = %#v, %v", keys, err)
	}
	if err := repo.CompleteSiteAssetCleanup(ctx, one.OpaqueKey); err != nil {
		t.Fatal(err)
	}
	keys, err = repo.SiteAssetCleanupKeys(ctx, 10)
	if err != nil || len(keys) != 0 {
		t.Fatalf("cleanup completion = %#v, %v", keys, err)
	}
}

func TestResetSiteAssetWithAuditQueuesPhysicalCleanup(t *testing.T) {
	ctx := context.Background()
	repo, err := db.Open(ctx, filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	actor, err := repo.CreateUser(ctx, db.User{Username: "actor", PasswordHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	key := "site/cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc.png"
	asset, err := repo.ReplaceSiteAsset(ctx, "nav_logo", db.SiteAsset{OpaqueKey: key, ContentType: "image/png", Size: 1, Width: 1, Height: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ResetSiteAssetWithAudit(ctx, "nav_logo", db.AuditEvent{UserID: actor.ID, Action: "admin.site_asset.reset", LogicalPath: "/nav_logo", Detail: "target_user_id=0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PublicSiteAssetByID(ctx, asset.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("reset asset remains: %v", err)
	}
	settings, err := repo.SiteSettings(ctx)
	if err != nil || settings.NavLogoAssetID != 0 {
		t.Fatalf("reset settings=%#v, %v", settings, err)
	}
	keys, err := repo.SiteAssetCleanupKeys(ctx, 10)
	if err != nil || len(keys) != 1 || keys[0] != key {
		t.Fatalf("reset cleanup=%#v, %v", keys, err)
	}
	events, err := repo.AuditEventsForUser(ctx, actor.ID)
	if err != nil || len(events) != 1 || events[0].Action != "admin.site_asset.reset" {
		t.Fatalf("reset audit=%#v, %v", events, err)
	}
}
