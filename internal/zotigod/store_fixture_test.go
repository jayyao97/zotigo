package zotigod

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jayyao97/zotigo/core/catalogschema"
	"github.com/jayyao97/zotigo/core/session"
)

// Build through the real migrations once, then close the only connection so
// SQLite checkpoints the WAL before we read the database. Never share a live
// database between tests, and never hand-maintain a second copy of the schema.
var emptyTestCatalog = sync.OnceValues(func() ([]byte, error) {
	root, err := os.MkdirTemp("", "zotigod-test-catalog-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)
	db, err := catalogschema.OpenDatabase(root)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	lease, err := catalogschema.Acquire(context.Background(), root, db, false)
	if err != nil {
		return nil, err
	}
	defer lease.Close()
	if err := db.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(root, "catalog.sqlite"))
})

// newTestStoreRoot is for business tests needing an empty, current catalog.
// Startup, migration, and legacy-import tests must keep using a fresh TempDir.
// Session bootstrap still runs normally when the test opens its FileStore.
func newTestStoreRoot(t *testing.T) string {
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

func TestStoreFixtureIsolation(t *testing.T) {
	first, err := session.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	sess, err := session.NewManagerWithStore(first).CreateNew(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, err := session.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	items, err := second.List(context.Background(), session.ListFilter{})
	if err != nil || len(items) != 0 {
		t.Fatalf("new fixture contains another store's sessions: %v, %v", items, err)
	}
	if _, err := session.NewManagerWithStore(second).CreateNew(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	items, err = first.List(context.Background(), session.ListFilter{})
	if err != nil || len(items) != 1 || items[0].ID != sess.ID {
		t.Fatalf("second store changed first store's sessions: %v, %v", items, err)
	}
}
