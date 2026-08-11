-- Old branding files are removed only after their replacement and audit event
-- commit. Failed filesystem removal is retried by the bounded cleanup runner.
CREATE TABLE IF NOT EXISTS site_asset_cleanup (
    opaque_key TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL
);
