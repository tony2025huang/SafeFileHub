CREATE TABLE IF NOT EXISTS directories (
    id INTEGER PRIMARY KEY,
    root_id INTEGER NOT NULL REFERENCES storage_roots(id) ON DELETE RESTRICT,
    logical_path TEXT NOT NULL,
    created_by_user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (root_id, logical_path)
);
CREATE INDEX IF NOT EXISTS directories_root_path_idx ON directories(root_id, logical_path);
