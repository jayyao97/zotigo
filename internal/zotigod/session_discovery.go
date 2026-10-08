package zotigod

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/jayyao97/zotigo/core/session"
)

type sessionDiscoveryQuery struct {
	Limit         int    `json:"limit"`
	AfterID       string `json:"after_id"`
	WorkspaceID   string `json:"workspace_id"`
	ProjectID     string `json:"project_id"`
	ActivitySince string `json:"activity_since"`
	ActivityUntil string `json:"activity_until"`
}

// Discovery is a read-only projection across the catalog and history index.
// Its private connection keeps ATTACH state out of both stores' writer pools.
func openSessionDiscovery(ctx context.Context, catalogRoot, sessionRoot string) (*sql.DB, error) {
	readOnlyURI := func(path string) string {
		return (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
	}
	db, err := sql.Open("sqlite", readOnlyURI(filepath.Join(catalogRoot, "catalog.sqlite")))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, `ATTACH DATABASE ? AS history`, readOnlyURI(filepath.Join(sessionRoot, "session_index.sqlite"))); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func sessionDiscoveryScope(caller sessionToolCaller, req sessionDiscoveryQuery) (string, []any) {
	where := `o.session_id > ? AND o.workspace_id IS NOT NULL AND o.self_archived_at IS NULL AND o.workspace_archived_at IS NULL`
	args := []any{req.AfterID}
	if caller.Origin != nil {
		where += ` AND o.workspace_id = ?`
		args = append(args, caller.WorkspaceID)
		if !caller.CanReadWorkspaceSessions {
			where += ` AND o.session_id = ?`
			args = append(args, caller.SessionID)
		}
	}
	if req.WorkspaceID != "" {
		where += ` AND o.workspace_id = ?`
		args = append(args, req.WorkspaceID)
	}
	if req.ProjectID != "" {
		where += ` AND o.project_id = ?`
		args = append(args, req.ProjectID)
	}
	return where, args
}

const discoveryFrom = ` FROM session_organization o JOIN history.sessions s ON s.id = o.session_id `

func sessionDiscoverySQL(where string, args []any, window session.DisplayTimeWindow, limit int) (string, []any) {
	matched := `NULL`
	var timeArgs []any
	if window.Since != nil || window.Until != nil {
		timeWhere := `d.session_id = o.session_id AND d.dialogue = 1`
		if window.Since != nil {
			timeWhere += ` AND d.message_at >= ?`
			timeArgs = append(timeArgs, window.Since.UnixNano())
		}
		if window.Until != nil {
			timeWhere += ` AND d.message_at < ?`
			timeArgs = append(timeArgs, window.Until.UnixNano())
		}
		matched = `(SELECT MAX(d.message_at) FROM history.display_items d WHERE ` + timeWhere + `)`
		where += ` AND EXISTS (SELECT 1 FROM history.display_items d WHERE ` + timeWhere + `)`
	}
	params := append([]any{}, timeArgs...)
	params = append(params, args...)
	params = append(params, timeArgs...)
	params = append(params, limit+1)
	return `SELECT o.session_id, COALESCE(o.title,''), o.workspace_id, COALESCE(o.project_id,''), s.agent, ` + matched + discoveryFrom + ` WHERE ` + where + ` ORDER BY o.session_id LIMIT ?`, params
}

// A time filter must not silently omit a log whose derived index is pending.
// Stream only file markers for authorized SQL candidates; do not load history
// or rebuild indexes on this request path. File stats preserve the existing
// readiness contract, including logs appended by another process.
func checkDiscoveryIndexes(ctx context.Context, tx *sql.Tx, root, where string, args []any) error {
	rows, err := tx.QueryContext(ctx, `SELECT o.session_id, f.observed_size, f.mtime`+discoveryFrom+` LEFT JOIN history.display_index_files f ON f.session_id=o.session_id WHERE `+where, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var id string
		var size, mtime sql.NullInt64
		if err := rows.Scan(&id, &size, &mtime); err != nil {
			return err
		}
		info, err := os.Stat(filepath.Join(root, "sessions", id+".display.jsonl"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !size.Valid || !mtime.Valid || size.Int64 != info.Size() || mtime.Int64 != info.ModTime().UnixNano() {
			return session.ErrDisplayIndexPending
		}
	}
	return rows.Err()
}

func (h *handler) discoverToolSessions(ctx context.Context, caller sessionToolCaller, req sessionDiscoveryQuery, window session.DisplayTimeWindow) ([]toolSessionSummary, string, error) {
	root := h.sessionStoreRoot()
	if root == "" {
		return nil, "", errors.New("session discovery requires indexed history storage")
	}
	db, err := openSessionDiscovery(ctx, h.catalog.RootDir(), root)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = db.Close() }()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback() }()
	where, args := sessionDiscoveryScope(caller, req)
	query, params := sessionDiscoverySQL(where, args, window, req.Limit)
	rows, err := tx.QueryContext(ctx, query, params...)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = rows.Close() }()
	result := make([]toolSessionSummary, 0, req.Limit)
	next := ""
	lookaheadID := ""
	for rows.Next() {
		var row toolSessionSummary
		var matched sql.NullInt64
		if err := rows.Scan(&row.ID, &row.Title, &row.WorkspaceID, &row.ProjectID, &row.Agent, &matched); err != nil {
			return nil, "", err
		}
		if len(result) == req.Limit {
			next = result[len(result)-1].ID
			lookaheadID = row.ID
			break
		}
		if matched.Valid {
			stamp := time.Unix(0, matched.Int64).UTC()
			row.LastMatchedMessageAt = &stamp
		}
		row.Title = boundedToolText(row.Title, 200)
		row.State = SessionStateOffline
		if live, ok := h.registry.Get(row.ID); ok {
			row.State = live.State
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if err := rows.Close(); err != nil {
		return nil, "", err
	}
	if window.Since != nil || window.Until != nil {
		// Validate even nonmatching candidates, since a stale index could hide a
		// match. Once a full page plus lookahead is found, later logs cannot
		// affect this page and need not be checked until a subsequent request.
		if lookaheadID != "" {
			where += ` AND o.session_id <= ?`
			args = append(args, lookaheadID)
		}
		if err := checkDiscoveryIndexes(ctx, tx, root, where, args); err != nil {
			return nil, "", err
		}
	}
	return result, next, nil
}
