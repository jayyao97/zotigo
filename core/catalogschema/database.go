package catalogschema

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	_ "modernc.org/sqlite"
)

// OpenDatabase configures every connection, including replacements made by
// database/sql. Catalog and session stores share the file, not a connection.
func OpenDatabase(root string) (*sql.DB, error) {
	path := filepath.Join(root, "catalog.sqlite")
	// Catalog transactions read before writing. Reserve the write lock at BEGIN
	// so worker commits cannot invalidate their snapshots (SQLITE_BUSY_SNAPSHOT
	// bypasses busy_timeout on a deferred read-to-write upgrade).
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// Acquire returns a shared schema lease, held until the store closes. Workers
// can open an initialized database while the daemon owns catalog.lock, but no
// schema change may run while any worker is using it. catalogLocked indicates
// that the caller already owns the lifetime catalog writer lock.
func Acquire(ctx context.Context, root string, db *sql.DB, catalogLocked bool) (*flock.Flock, error) {
	lease := flock.New(filepath.Join(root, "schema.lock"))
	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	locked, err := lease.TryRLockContext(lockCtx, 10*time.Millisecond)
	if err != nil || !locked {
		_ = lease.Close()
		return nil, fmt.Errorf("database schema is in use: %w", errors.Join(err, lockCtx.Err()))
	}
	isReady, err := ready(ctx, db)
	if err != nil || isReady {
		if err != nil {
			_ = lease.Close()
			return nil, err
		}
		return lease, nil
	}
	_ = lease.Close()
	if !catalogLocked {
		writer := flock.New(filepath.Join(root, "catalog.lock"))
		defer func() { _ = writer.Close() }()
		locked, err := writer.TryLockContext(lockCtx, 10*time.Millisecond)
		if err != nil || !locked {
			return nil, fmt.Errorf("catalog is in use; stop zotigod before upgrading: %w", errors.Join(err, lockCtx.Err()))
		}
	}
	lease = flock.New(filepath.Join(root, "schema.lock"))
	// A concurrent first opener may have completed the upgrade while we waited
	// for catalog.lock and may already hold a shared lease.
	isReady, err = ready(ctx, db)
	if err != nil {
		return nil, err
	}
	if isReady {
		if err := lease.RLock(); err != nil {
			_ = lease.Close()
			return nil, err
		}
		return lease, nil
	}
	locked, err = lease.TryLockContext(lockCtx, 10*time.Millisecond)
	if err != nil || !locked {
		_ = lease.Close()
		return nil, errors.New("database schema is in use; stop daemon and session workers before migrating")
	}
	if err := upgrade(ctx, db, root); err != nil {
		_ = lease.Close()
		return nil, err
	}
	// Keep catalog.lock through the handoff, so another migrator cannot slip
	// between the exclusive upgrade and the store's shared lease.
	if err := lease.Unlock(); err != nil {
		_ = lease.Close()
		return nil, err
	}
	if err := lease.RLock(); err != nil {
		_ = lease.Close()
		return nil, err
	}
	return lease, nil
}

func ready(ctx context.Context, db *sql.DB) (bool, error) {
	version, err := CatalogVersion(ctx, db)
	if err != nil {
		return false, err
	}
	if version > Version || version < 0 {
		return false, fmt.Errorf("workspace catalog version %d is not supported", version)
	}
	if version != Version {
		return false, nil
	}
	var recorded int
	var dirty bool
	if err := db.QueryRowContext(ctx, "SELECT version, dirty FROM catalog_migrations").Scan(&recorded, &dirty); err != nil {
		return false, fmt.Errorf("read catalog migration journal: %w", err)
	}
	if dirty {
		return false, fmt.Errorf("catalog migration %d is dirty; restore a consistent backup before retrying", recorded)
	}
	if recorded != version {
		return false, fmt.Errorf("catalog migration journal version %d differs from schema version %d", recorded, version)
	}
	var imported bool
	err = db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM metadata WHERE key=? AND value='1')", importMarker).Scan(&imported)
	return imported, err
}

func upgrade(ctx context.Context, db *sql.DB, root string) error {
	if err := Migrate(ctx, db, Version); err != nil {
		return err
	}
	return importSessionIndex(ctx, db, root)
}

// MigrateOffline requires the caller to hold catalog.lock. Refuse a destructive
// downgrade of populated session tables; restoring a pre-upgrade backup is the
// rollback path, rather than silently discarding newly written session data.
func MigrateOffline(ctx context.Context, db *sql.DB, root string, target uint) error {
	lease := flock.New(filepath.Join(root, "schema.lock"))
	defer func() { _ = lease.Close() }()
	locked, err := lease.TryLock()
	if err != nil {
		return err
	}
	if !locked {
		return errors.New("database schema is in use; stop daemon and session workers before migrating")
	}
	version, err := CatalogVersion(ctx, db)
	if err != nil {
		return err
	}
	if version >= 10 && target < 10 {
		if _, err := os.Stat(filepath.Join(root, "session_index.sqlite")); err == nil {
			return errors.New("cannot downgrade with a legacy session index backup; restore the complete pre-upgrade backup instead")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for _, table := range sessionTables {
			var populated bool
			query := `SELECT EXISTS(SELECT 1 FROM "` + table + `"`
			if table == "metadata" {
				query += ` WHERE key NOT IN ('` + importMarker + `', 'bootstrapped')`
			}
			if err := db.QueryRowContext(ctx, query+")").Scan(&populated); err != nil {
				return err
			}
			if populated {
				return errors.New("cannot downgrade populated unified session storage; stop all processes and restore a pre-upgrade backup")
			}
		}
	}
	if target == Version {
		return upgrade(ctx, db, root)
	}
	return Migrate(ctx, db, target)
}
