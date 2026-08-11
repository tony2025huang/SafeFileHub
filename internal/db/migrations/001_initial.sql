CREATE TABLE IF NOT EXISTS users (
    id INTEGER PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    disabled INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0, 1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS storage_roots (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    path TEXT NOT NULL UNIQUE,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS files (
    id INTEGER PRIMARY KEY,
    root_id INTEGER NOT NULL REFERENCES storage_roots(id) ON DELETE RESTRICT,
    logical_path TEXT NOT NULL,
    object_key TEXT NOT NULL UNIQUE,
    size INTEGER NOT NULL CHECK (size >= 0),
    created_by_user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (root_id, logical_path)
);

CREATE TABLE IF NOT EXISTS permissions (
    id INTEGER PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    root_id INTEGER NOT NULL REFERENCES storage_roots(id) ON DELETE CASCADE,
    path_prefix TEXT NOT NULL,
    action TEXT NOT NULL,
    allow INTEGER NOT NULL CHECK (allow IN (0, 1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (user_id, root_id, path_prefix, action)
);

CREATE TABLE IF NOT EXISTS upload_sessions (
    id TEXT PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    root_id INTEGER NOT NULL REFERENCES storage_roots(id) ON DELETE RESTRICT,
    logical_path TEXT NOT NULL,
    staging_path TEXT NOT NULL UNIQUE,
    offset INTEGER NOT NULL DEFAULT 0 CHECK (offset >= 0),
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_events (
    id INTEGER PRIMARY KEY,
    user_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
    root_id INTEGER REFERENCES storage_roots(id) ON DELETE SET NULL,
    action TEXT NOT NULL,
    logical_path TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS files_root_path_idx ON files(root_id, logical_path);
CREATE INDEX IF NOT EXISTS permissions_user_root_idx ON permissions(user_id, root_id);
CREATE INDEX IF NOT EXISTS upload_sessions_expires_idx ON upload_sessions(expires_at);
CREATE INDEX IF NOT EXISTS audit_events_user_created_idx ON audit_events(user_id, created_at DESC);
