package workspace

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// PathWithin reports whether path is root or one of its descendants. Both
// the lexical form and the symlink-resolved form are accepted, so a session
// started through a symlinked directory and one started from the resolved
// directory classify the same way. Paths must be absolute.
func PathWithin(root string, path string) bool {
	if !filepath.IsAbs(root) || !filepath.IsAbs(path) {
		return false
	}
	if lexicallyWithin(filepath.Clean(root), filepath.Clean(path)) {
		return true
	}
	resolvedRoot, rootErr := filepath.EvalSymlinks(root)
	resolvedPath, pathErr := filepath.EvalSymlinks(path)
	if rootErr != nil || pathErr != nil {
		return false
	}
	return lexicallyWithin(resolvedRoot, resolvedPath)
}

func lexicallyWithin(root string, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// WorkspaceContainingPath returns the non-deleted workspace whose root
// contains path. It returns ErrNotFound when no workspace owns the path and
// ErrConflict when more than one does; callers must not guess in that case.
// The returned workspace may be in any non-deleted status so callers can
// reject an archived owner instead of silently creating an unowned session
// inside its directory.
func (s *Store) WorkspaceContainingPath(ctx context.Context, path string) (Workspace, error) {
	if !filepath.IsAbs(path) {
		return Workspace{}, fmt.Errorf("%w: path must be absolute", ErrInvalid)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, title, storage_name, root_path, status, error,
		       archived_at, deleted_at, created_at, updated_at
		FROM workspaces WHERE status != 'deleted'
		ORDER BY id ASC
	`)
	if err != nil {
		return Workspace{}, fmt.Errorf("list workspaces: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var matches []Workspace
	for rows.Next() {
		workspace, err := scanWorkspace(rows)
		if err != nil {
			return Workspace{}, err
		}
		if PathWithin(workspace.RootPath, path) {
			matches = append(matches, workspace)
		}
	}
	if err := rows.Err(); err != nil {
		return Workspace{}, fmt.Errorf("list workspaces: %w", err)
	}
	switch len(matches) {
	case 0:
		return Workspace{}, ErrNotFound
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, 0, len(matches))
		for _, match := range matches {
			ids = append(ids, match.ID)
		}
		return Workspace{}, fmt.Errorf("%w: path %s belongs to multiple workspaces (%s)", ErrConflict, path, strings.Join(ids, ", "))
	}
}
