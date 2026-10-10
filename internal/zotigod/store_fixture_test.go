package zotigod

import (
	"context"
	"testing"

	"github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/testutil/catalogtest"
)

func newTestStoreRoot(t *testing.T) string {
	t.Helper()
	return catalogtest.NewRoot(t)
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
