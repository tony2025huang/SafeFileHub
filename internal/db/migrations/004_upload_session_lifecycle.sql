ALTER TABLE upload_sessions ADD COLUMN status TEXT NOT NULL DEFAULT 'active'
    CHECK (status IN ('active', 'cancelled', 'cleanup_pending', 'complete'));
