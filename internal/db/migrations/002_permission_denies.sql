-- Permit explicit allow and deny records at an equal prefix so authorization
-- can resolve ties safely (deny wins) without relying on insertion order.
CREATE TABLE permissions_new (
    id INTEGER PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    root_id INTEGER NOT NULL REFERENCES storage_roots(id) ON DELETE CASCADE,
    path_prefix TEXT NOT NULL,
    action TEXT NOT NULL,
    allow INTEGER NOT NULL CHECK (allow IN (0, 1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (user_id, root_id, path_prefix, action, allow)
);
INSERT INTO permissions_new (id, user_id, root_id, path_prefix, action, allow, created_at, updated_at)
SELECT id, user_id, root_id, path_prefix, action, allow, created_at, updated_at FROM permissions;
DROP TABLE permissions;
ALTER TABLE permissions_new RENAME TO permissions;
CREATE INDEX IF NOT EXISTS permissions_user_root_idx ON permissions(user_id, root_id);
