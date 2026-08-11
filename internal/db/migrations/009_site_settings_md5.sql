-- Site configuration is a fixed singleton. Its defaults preserve the behaviour
-- of deployments created before this migration: SafeFileHub branding, no filing
-- display, no custom assets, and MD5 disabled.
CREATE TABLE IF NOT EXISTS site_assets (
    id INTEGER PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('login_logo', 'nav_logo', 'favicon')),
    opaque_key TEXT NOT NULL UNIQUE,
    content_type TEXT NOT NULL,
    size INTEGER NOT NULL CHECK (size >= 0),
    width INTEGER NOT NULL CHECK (width > 0),
    height INTEGER NOT NULL CHECK (height > 0),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS site_settings (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    site_name TEXT NOT NULL DEFAULT 'SafeFileHub',
    primary_color TEXT NOT NULL DEFAULT '#2563EB',
    filing_enabled INTEGER NOT NULL DEFAULT 0 CHECK (filing_enabled IN (0, 1)),
    filing_text TEXT NOT NULL DEFAULT '',
    md5_enabled INTEGER NOT NULL DEFAULT 0 CHECK (md5_enabled IN (0, 1)),
    login_logo_asset_id INTEGER REFERENCES site_assets(id) ON DELETE SET NULL,
    nav_logo_asset_id INTEGER REFERENCES site_assets(id) ON DELETE SET NULL,
    favicon_asset_id INTEGER REFERENCES site_assets(id) ON DELETE SET NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
INSERT INTO site_settings (id, created_at, updated_at) VALUES (1, 0, 0)
ON CONFLICT(id) DO NOTHING;

-- Existing files deliberately remain disabled: enabling MD5 later applies only
-- to subsequently completed uploads and never schedules a historical backfill.
ALTER TABLE files ADD COLUMN md5_status TEXT NOT NULL DEFAULT 'disabled'
    CHECK (md5_status IN ('disabled', 'pending', 'computing', 'ready', 'failed'));
ALTER TABLE files ADD COLUMN md5_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE files ADD COLUMN md5_error TEXT NOT NULL DEFAULT '';

-- A file has at most one durable MD5 work item. Retry state is explicitly
-- bounded, and the FK makes file deletion clean up an unclaimed/claimed task.
CREATE TABLE IF NOT EXISTS md5_tasks (
    file_id INTEGER PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'computing', 'failed', 'complete')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts INTEGER NOT NULL DEFAULT 3 CHECK (max_attempts > 0 AND max_attempts <= 10 AND attempts <= max_attempts),
    available_at INTEGER NOT NULL,
    claimed_at INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS md5_tasks_claim_idx ON md5_tasks(status, available_at, file_id);
