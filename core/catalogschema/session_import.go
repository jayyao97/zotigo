package catalogschema

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const importMarker = "catalog_session_import_v10"

var sessionTables = []string{"sessions", "session_images", "metadata", "display_items", "display_index_files"}

// The legacy file is read through SQLite (including its WAL), never copied as
// raw bytes or modified. Rows and completion marker commit together. A failed
// import is retryable even if the schema migration has already committed.
func importSessionIndex(ctx context.Context, db *sql.DB, root string) error {
	var done bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM metadata WHERE key=? AND value='1')", importMarker).Scan(&done); err != nil || done {
		return err
	}
	path := filepath.Join(root, "session_index.sqlite")
	_, statErr := os.Stat(path)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if statErr == nil {
		uri := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
		if _, err := db.ExecContext(ctx, "ATTACH DATABASE ? AS legacy_sessions", uri); err != nil {
			return fmt.Errorf("open legacy session index: %w", err)
		}
		defer func() { _, _ = db.ExecContext(context.Background(), "DETACH DATABASE legacy_sessions") }()
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if statErr == nil {
		for _, table := range sessionTables {
			var exists bool
			if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM legacy_sessions.sqlite_master WHERE type='table' AND name=?)", table).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				if table == "sessions" {
					return errors.New("legacy session index has no sessions table")
				}
				continue
			}
			// Older indexes lack optional columns. Copy their intersection with
			// the versioned schema, letting SQL defaults fill only missing fields.
			rows, err := tx.QueryContext(ctx, `SELECT d.name FROM pragma_table_info(?, 'main') d
JOIN pragma_table_info(?, 'legacy_sessions') s ON s.name=d.name ORDER BY d.cid`, table, table)
			if err != nil {
				return err
			}
			var columns []string
			for rows.Next() {
				var name string
				if err := rows.Scan(&name); err != nil {
					_ = rows.Close()
					return err
				}
				columns = append(columns, `"`+strings.ReplaceAll(name, `"`, `""`)+`"`)
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return err
			}
			names := strings.Join(columns, ",")
			query := `INSERT INTO main."` + table + `" (` + names + `) SELECT ` + names + ` FROM legacy_sessions."` + table + `"`
			if _, err := tx.ExecContext(ctx, query); err != nil {
				return fmt.Errorf("import legacy %s: %w", table, err)
			}
		}
	}
	// A direct upgrade from the separate legacy index imports offsets after SQL
	// migrations have run. Those logs still need their content classification.
	if _, err := tx.ExecContext(ctx, `DELETE FROM display_index_files WHERE session_id IN
(SELECT session_id FROM display_items WHERE content_kind='unknown')`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES(?, '1')", importMarker); err != nil {
		return err
	}
	return tx.Commit()
}
