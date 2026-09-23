package workspace

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type NavigationItem struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type NavigationOrder struct {
	Scope    string           `json:"scope"`
	ParentID string           `json:"parent_id,omitempty"`
	Items    []NavigationItem `json:"items"`
}

type Navigation struct {
	LegacyOrderImported bool             `json:"legacy_order_imported"`
	Projects            []NavigationItem `json:"projects"`
	Workspaces          []NavigationItem `json:"workspaces"`
	Pinned              []NavigationItem `json:"pinned"`
}

// All SQL identifiers are selected here, never interpolated from client input.
func navigationTable(kind string) (string, string, error) {
	switch kind {
	case "project":
		return "projects", "id", nil
	case "workspace":
		return "workspaces", "id", nil
	case "session":
		return "session_organization", "session_id", nil
	default:
		return "", "", ErrInvalid
	}
}

const pinnedNavigationQuery = `SELECT kind, id FROM (
 SELECT 'project' AS kind, id, pinned_position, pinned_at FROM projects WHERE pinned_at IS NOT NULL AND status = 'active'
 UNION ALL SELECT 'workspace', id, pinned_position, pinned_at FROM workspaces WHERE pinned_at IS NOT NULL AND status IN ('ready', 'error', 'provisioning')
 UNION ALL SELECT 'session', session_id, pinned_position, pinned_at FROM session_organization WHERE pinned_at IS NOT NULL AND self_archived_at IS NULL AND workspace_archived_at IS NULL
) ORDER BY pinned_position, pinned_at, kind, id`

type navigationQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readNavigationItems(ctx context.Context, db navigationQuerier, query string, args ...any) ([]NavigationItem, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := []NavigationItem{}
	for rows.Next() {
		var item NavigationItem
		if err := rows.Scan(&item.Kind, &item.ID); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) GetNavigation(ctx context.Context) (Navigation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Navigation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var result Navigation
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM navigation_migration)`).Scan(&result.LegacyOrderImported); err != nil {
		return result, err
	}
	result.Projects, err = readNavigationItems(ctx, tx, `SELECT 'project', id FROM projects WHERE status = 'active' ORDER BY position IS NULL DESC, position, created_at DESC, id`)
	if err != nil {
		return result, err
	}
	result.Workspaces, err = readNavigationItems(ctx, tx, `SELECT 'workspace', id FROM workspaces WHERE status IN ('ready', 'error', 'provisioning') ORDER BY position IS NULL DESC, position, created_at DESC, id`)
	if err != nil {
		return result, err
	}
	result.Pinned, err = readNavigationItems(ctx, tx, pinnedNavigationQuery)
	return result, err
}

// Reordering a visible subset keeps hidden pinned nodes in their original slots.
// Validation and all position writes share a transaction, so stale/deleted items
// cannot leave half of a drag applied.
func (s *Store) ReorderNavigation(ctx context.Context, scope, parentID string, items []NavigationItem) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := reorderNavigation(ctx, tx, scope, parentID, items, false); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO navigation_migration(singleton) VALUES(1)`); err != nil {
		return err
	}
	return tx.Commit()
}

func reorderNavigation(ctx context.Context, tx *sql.Tx, scope, parentID string, items []NavigationItem, importing bool) error {
	var current []NavigationItem
	var err error
	switch scope {
	case "projects":
		if parentID != "" {
			return ErrInvalid
		}
		current, err = readNavigationItems(ctx, tx, `SELECT 'project', id FROM projects WHERE status = 'active' ORDER BY position IS NULL DESC, position, created_at DESC, id`)
	case "workspaces":
		if parentID == "" {
			return ErrInvalid
		}
		current, err = readNavigationItems(ctx, tx, `SELECT 'workspace', id FROM workspaces WHERE project_id = ? AND status IN ('ready', 'error', 'provisioning') ORDER BY position IS NULL DESC, position, created_at DESC, id`, parentID)
	case "sessions":
		if parentID == "" {
			return ErrInvalid
		}
		current, err = readNavigationItems(ctx, tx, `SELECT 'session', session_id FROM session_organization WHERE workspace_id = ? AND self_archived_at IS NULL AND workspace_archived_at IS NULL ORDER BY workspace_position IS NULL, workspace_position, activity_at DESC, session_id`, parentID)
	case "pinned":
		if parentID != "" {
			return ErrInvalid
		}
		current, err = readNavigationItems(ctx, tx, pinnedNavigationQuery)
	default:
		return ErrInvalid
	}
	if err != nil {
		return err
	}
	available := make(map[NavigationItem]bool, len(current))
	for _, item := range current {
		available[item] = true
	}
	if importing {
		// Local preferences can still mention deleted or unpinned items.
		filtered := make([]NavigationItem, 0, len(items))
		seen := map[NavigationItem]bool{}
		for _, item := range items {
			if available[item] && !seen[item] {
				filtered = append(filtered, item)
				seen[item] = true
			}
		}
		items = filtered
	}
	selected := make(map[NavigationItem]bool, len(items))
	for _, item := range items {
		if !available[item] {
			return fmt.Errorf("%w: navigation item is no longer in this list", ErrConflict)
		}
		if selected[item] {
			return fmt.Errorf("%w: duplicate navigation item", ErrInvalid)
		}
		selected[item] = true
	}
	next := 0
	for index, item := range current {
		if selected[item] {
			item = items[next]
			next++
		}
		table, idColumn, err := navigationTable(item.Kind)
		if err != nil {
			return err
		}
		column := "position"
		if scope == "sessions" {
			column = "workspace_position"
		}
		if scope == "pinned" {
			column = "pinned_position"
		}
		extra := ""
		if item.Kind == "session" {
			extra = ", revision = revision + 1"
		}
		query := fmt.Sprintf(`UPDATE %s SET %s = ?%s WHERE %s = ?`, table, column, extra, idColumn)
		if _, err := tx.ExecContext(ctx, query, (index+1)*1000, item.ID); err != nil {
			return err
		}
	}
	return nil
}

// Only the first client imports its old host-scoped UI orders. A manual daemon
// reorder also closes this migration, preventing late clients from overwriting it.
func (s *Store) ImportNavigation(ctx context.Context, orders []NavigationOrder) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var imported bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM navigation_migration)`).Scan(&imported); err != nil {
		return err
	}
	if imported {
		return nil
	}
	for _, order := range orders {
		if order.Scope == "sessions" {
			return ErrInvalid
		}
		if err := reorderNavigation(ctx, tx, order.Scope, order.ParentID, order.Items, true); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO navigation_migration(singleton) VALUES(1)`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetNavigationPinned(ctx context.Context, item NavigationItem, pinned bool) error {
	table, idColumn, err := navigationTable(item.Kind)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	condition := "status = 'active'"
	if item.Kind == "workspace" {
		condition = "status IN ('ready', 'error', 'provisioning')"
	}
	if item.Kind == "session" {
		condition = "self_archived_at IS NULL AND workspace_archived_at IS NULL"
	}
	var active bool
	var previous sql.NullInt64
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT (%s), pinned_at FROM %s WHERE %s = ?`, condition, table, idColumn), item.ID).Scan(&active, &previous); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return err
	}
	if !active {
		return fmt.Errorf("%w: inactive item cannot be pinned", ErrConflict)
	}
	if previous.Valid == pinned {
		return nil
	}
	var pinnedAt, position any
	if pinned {
		var last int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), 0) + 1000 FROM (
		 SELECT pinned_position AS position FROM projects UNION ALL SELECT pinned_position FROM workspaces UNION ALL SELECT pinned_position FROM session_organization
		)`).Scan(&last); err != nil {
			return err
		}
		pinnedAt, position = unixMillis(time.Now().UTC()), last
	}
	extra := ""
	if item.Kind == "session" {
		extra = ", revision = revision + 1"
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET pinned_at = ?, pinned_position = ?, updated_at = ?%s WHERE %s = ?`, table, extra, idColumn), pinnedAt, position, unixMillis(time.Now().UTC()), item.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}
