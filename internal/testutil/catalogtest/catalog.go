// Package catalogtest provides isolated catalogs for business tests.
package catalogtest

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jayyao97/zotigo/core/catalogschema"
)

// Build through the real migrations once, then close the only connection so
// SQLite checkpoints the WAL before we read the database. Never share a live
// database between tests, and never hand-maintain a second copy of the schema.
var emptyTestCatalog = sync.OnceValues(func() ([]byte, error) {
	root, err := os.MkdirTemp("", "zotigo-test-catalog-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(root) }()
	db, err := catalogschema.OpenDatabase(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	lease, err := catalogschema.Acquire(context.Background(), root, db, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lease.Close() }()
	if err := db.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(root, "catalog.sqlite"))
})

// NewRoot is for business tests needing an empty, current catalog.
// Startup, migration, and legacy-import tests must keep using a fresh TempDir.
// Session bootstrap still runs normally when the test opens its FileStore.
func NewRoot(t testing.TB) string {
	t.Helper()
	data, err := emptyTestCatalog()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "catalog.sqlite"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}
