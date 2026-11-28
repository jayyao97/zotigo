-- atlas:delimiter \n\n

ALTER TABLE session_organization ADD COLUMN deleted_workspace_id TEXT;

-- Older deletions stamped the detached organization and workspace in one
-- transaction. Require the matching directory as independent evidence.
UPDATE session_organization AS o SET deleted_workspace_id = (
  SELECT min(w.id)
  FROM sessions s JOIN workspaces w ON w.status = 'deleted' AND w.deleted_at = o.updated_at
  WHERE s.id = o.session_id
    AND instr(s.working_directory || '/', '/../') = 0
    AND instr(s.working_directory || '/', w.root_path || '/') = 1
  HAVING count(*) = 1
) WHERE workspace_id IS NULL AND self_archived_at IS NOT NULL;

-- Record the actual prior assignment even when a compatible older daemon
-- performs deletion. The historical ID survives deletion of the project.
CREATE TRIGGER record_deleted_session_workspace AFTER UPDATE OF workspace_id ON session_organization
WHEN NEW.workspace_id IS NULL AND NEW.self_archived_at IS NOT NULL
  AND EXISTS (SELECT 1 FROM workspaces WHERE id = OLD.workspace_id AND status = 'deleting')
BEGIN
  UPDATE session_organization SET deleted_workspace_id = OLD.workspace_id WHERE session_id = NEW.session_id;
END;
