-- Directory parent existence is enforced in SQLite, not just by HTTP preflight
-- checks. This closes create-child versus delete-parent races between
-- independent connections/processes: whichever writer commits first makes the
-- other operation fail without leaving an orphaned directory descendant.
-- Files remain compatible with legacy upload APIs, whose parent semantics are
-- intentionally managed at their request layer.
CREATE TRIGGER IF NOT EXISTS directories_parent_directory_exists BEFORE INSERT ON directories
WHEN instr(substr(NEW.logical_path, 2), '/') > 0
     AND NOT EXISTS (
         SELECT 1 FROM directories
         WHERE root_id = NEW.root_id AND NEW.logical_path LIKE logical_path || '/%'
     )
BEGIN SELECT RAISE(ABORT, 'parent directory not found'); END;
