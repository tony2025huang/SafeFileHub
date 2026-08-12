-- The bootstrap administrator is an explicit singleton identity, not an
-- incidental users.id value.  Deliberately do not backfill an existing user:
-- old databases cannot safely distinguish an ordinary first user from a
-- historical bootstrap account.
CREATE TABLE IF NOT EXISTS bootstrap_admin (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    user_id INTEGER NOT NULL UNIQUE REFERENCES users(id) ON DELETE RESTRICT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
