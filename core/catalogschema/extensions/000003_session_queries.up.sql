CREATE INDEX idx_sessions_activity_page ON sessions(updated_at DESC, id ASC);
CREATE INDEX idx_session_organization_workspace_page ON session_organization(workspace_id, workspace_position, session_id);
CREATE INDEX idx_session_read_state_unread ON session_read_state(session_id) WHERE version > read_version;
CREATE INDEX idx_session_organization_workspace_seek ON session_organization(workspace_id, session_id);
