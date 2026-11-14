package workspace

import (
	"context"
	"encoding/json"
	"strings"
)

// SessionQuery selects navigation rows before their runtime DTOs are assembled.
// ChannelIDs are binding keys from the channel store, never message histories.
type SessionQuery struct {
	ProjectID, WorkspaceID, Search, ChannelMode string
	IDs, ChannelIDs                             []string
	Ungrouped, Pinned, IncludeArchived          bool
	Unread                                      *bool
	Since, Before                               int64
	BeforeID                                    string
	BeforePosition                              int64
	Limit                                       int
}

type SessionQueryRow struct {
	ID        string
	UpdatedAt int64
	Position  int64
}

func sessionQuerySQL(filter SessionQuery) (string, []any) {
	position := "(0+0)"
	if filter.WorkspaceID != "" {
		position = "COALESCE(o.workspace_position,9223372036854775807)"
	}
	from := "sessions s"
	readJoin := " LEFT JOIN session_read_state r ON r.session_id=s.id"
	if filter.Unread != nil && *filter.Unread {
		// Keep sparse unread polls proportional to unread candidates, not history.
		from = "session_read_state r INDEXED BY idx_session_read_state_unread CROSS JOIN sessions s ON s.id=r.session_id"
		readJoin = ""
	}
	query := `SELECT s.id, s.updated_at, ` + position + ` FROM ` + from + `
 LEFT JOIN session_organization o ON o.session_id=s.id
 LEFT JOIN workspaces w ON w.id=o.workspace_id
 LEFT JOIN projects p ON p.id=o.project_id` + readJoin + ` WHERE 1=1`
	args := []any{}
	add := func(clause string, values ...any) { query += " AND " + clause; args = append(args, values...) }
	if !filter.IncludeArchived {
		add(`o.self_archived_at IS NULL AND o.workspace_archived_at IS NULL`)
	}
	if filter.ProjectID != "" {
		add(`o.project_id=?`, filter.ProjectID)
	}
	if filter.WorkspaceID != "" {
		add(`o.workspace_id=?`, filter.WorkspaceID)
	}
	if filter.Ungrouped {
		add(`o.workspace_id IS NULL`)
	}
	if filter.Pinned {
		add(`o.pinned_at IS NOT NULL`)
	}
	if filter.Unread != nil {
		if *filter.Unread {
			add(`r.version>r.read_version`)
		} else {
			add(`COALESCE(r.version,0)=COALESCE(r.read_version,0)`)
		}
	}
	if filter.Since > 0 {
		add(`s.updated_at>=?`, filter.Since)
	}
	if filter.BeforeID != "" {
		if filter.WorkspaceID != "" {
			add(`(`+position+`>? OR (`+position+`=? AND (s.updated_at<? OR (s.updated_at=? AND s.id>?))))`, filter.BeforePosition, filter.BeforePosition, filter.Before, filter.Before, filter.BeforeID)
		} else {
			add(`(s.updated_at<? OR (s.updated_at=? AND s.id>?))`, filter.Before, filter.Before, filter.BeforeID)
		}
	}
	if filter.IDs != nil {
		ids, _ := json.Marshal(filter.IDs)
		add(`s.id IN (SELECT value FROM json_each(?))`, string(ids))
	}
	if filter.ChannelMode != "all" && len(filter.ChannelIDs) > 0 {
		ids, _ := json.Marshal(filter.ChannelIDs)
		clause := `s.id NOT IN (SELECT value FROM json_each(?))`
		if filter.ChannelMode == "pinned" {
			clause = `(` + clause + ` OR o.pinned_at IS NOT NULL OR w.pinned_at IS NOT NULL OR p.pinned_at IS NOT NULL)`
		}
		add(clause, string(ids))
	}
	for _, search := range strings.Fields(filter.Search) {
		add(`(instr(lower(COALESCE(o.title,'') || ' ' || s.last_prompt || ' ' || COALESCE(w.title,'') || ' ' || COALESCE(p.name,'')),lower(?))>0)`, search)
	}
	if filter.WorkspaceID != "" {
		query += ` ORDER BY ` + position + ` ASC, s.updated_at DESC, s.id ASC LIMIT ?`
	} else {
		query += ` ORDER BY s.updated_at DESC, s.id ASC LIMIT ?`
	}
	args = append(args, filter.Limit)
	return query, args
}

func (s *Store) QuerySessions(ctx context.Context, filter SessionQuery) ([]SessionQueryRow, error) {
	query, args := sessionQuerySQL(filter)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]SessionQueryRow, 0)
	for rows.Next() {
		var row SessionQueryRow
		if err := rows.Scan(&row.ID, &row.UpdatedAt, &row.Position); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}
