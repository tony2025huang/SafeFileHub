ALTER TABLE upload_sessions ADD COLUMN length INTEGER NOT NULL DEFAULT 0 CHECK (length >= 0);
