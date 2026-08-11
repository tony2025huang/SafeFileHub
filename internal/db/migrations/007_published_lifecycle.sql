ALTER TABLE files ADD COLUMN delete_state TEXT NOT NULL DEFAULT 'active' CHECK (delete_state IN ('active','deleting'));
CREATE TABLE IF NOT EXISTS object_cleanup_jobs (
    object_key TEXT PRIMARY KEY,
    reason TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
-- A path is globally occupied within a root, irrespective of its kind.  The
-- parent and non-empty checks are triggers so independent HTTP processes get
-- the same invariant as in-process callers.
CREATE TRIGGER IF NOT EXISTS files_path_not_directory BEFORE INSERT ON files
WHEN EXISTS (SELECT 1 FROM directories WHERE root_id=NEW.root_id AND logical_path=NEW.logical_path)
BEGIN SELECT RAISE(ABORT, 'published path occupied by directory'); END;
CREATE TRIGGER IF NOT EXISTS directories_path_not_file BEFORE INSERT ON directories
WHEN EXISTS (SELECT 1 FROM files WHERE root_id=NEW.root_id AND logical_path=NEW.logical_path)
BEGIN SELECT RAISE(ABORT, 'published path occupied by file'); END;
CREATE TRIGGER IF NOT EXISTS directories_must_be_empty BEFORE DELETE ON directories
WHEN EXISTS (SELECT 1 FROM files WHERE root_id=OLD.root_id AND logical_path LIKE OLD.logical_path || '/%' UNION ALL SELECT 1 FROM directories WHERE root_id=OLD.root_id AND logical_path LIKE OLD.logical_path || '/%')
BEGIN SELECT RAISE(ABORT, 'directory not empty'); END;
