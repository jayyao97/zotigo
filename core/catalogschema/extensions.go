package catalogschema

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
)

// ExtensionVersion only advances for changes that preserve dev.31's reads and
// writes. New executable queue payloads must never enter its runnable states.
const ExtensionVersion = 3

//go:embed extensions/*.sql
var extensions embed.FS

func extensionVersion(ctx context.Context, db *sql.DB) (int, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='catalog_extensions')`).Scan(&exists); err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	var version int
	err := db.QueryRowContext(ctx, `SELECT version FROM catalog_extensions WHERE singleton=1`).Scan(&version)
	return version, err
}

func CheckExtensions(ctx context.Context, db *sql.DB) error {
	version, err := extensionVersion(ctx, db)
	if err != nil {
		return err
	}
	if version < ExtensionVersion {
		return fmt.Errorf("catalog extensions %d are not ready (expected %d)", version, ExtensionVersion)
	}
	return nil
}

func ensureExtensions(ctx context.Context, db *sql.DB) error {
	version, err := extensionVersion(ctx, db)
	if err != nil {
		return err
	}
	// Compatible future additions must survive binary rollback. Startup only
	// fills missing extensions; it never downgrades an unfamiliar journal.
	if version >= ExtensionVersion {
		return nil
	}
	return migrateExtensions(ctx, db, ExtensionVersion)
}

// Each extension and its progress marker commit in one SQLite transaction.
// A failed addition leaves a database that the old daemon can still open.
// Caller holds catalog.lock and the exclusive schema lease.
func migrateExtensions(ctx context.Context, db *sql.DB, target int) error {
	version, err := extensionVersion(ctx, db)
	if err != nil {
		return err
	}
	if version < 0 || version > ExtensionVersion || target < 0 || target > ExtensionVersion {
		return fmt.Errorf("unsupported catalog extension migration %d -> %d", version, target)
	}
	for version != target {
		number, next, direction := version+1, version+1, "up"
		if version > target {
			number, next, direction = version, version-1, "down"
		}
		paths, err := fs.Glob(extensions, fmt.Sprintf("extensions/%06d_*.%s.sql", number, direction))
		if err != nil {
			return err
		}
		if len(paths) != 1 {
			return fmt.Errorf("missing catalog extension %d %s", number, direction)
		}
		body, err := extensions.ReadFile(paths[0])
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		err = func() error {
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS catalog_extensions(singleton INTEGER PRIMARY KEY CHECK(singleton=1), version INTEGER NOT NULL CHECK(version>=0))`); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_extensions(singleton,version) VALUES(1,?) ON CONFLICT(singleton) DO UPDATE SET version=excluded.version`, next); err != nil {
				return err
			}
			if next == 0 {
				if _, err := tx.ExecContext(ctx, `DROP TABLE catalog_extensions`); err != nil {
					return err
				}
			}
			var violations int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
				return err
			}
			if violations != 0 {
				return fmt.Errorf("catalog extension introduced %d foreign key violations", violations)
			}
			return tx.Commit()
		}()
		if err != nil {
			return fmt.Errorf("catalog extension %d %s: %w", number, direction, err)
		}
		version = next
	}
	return nil
}
