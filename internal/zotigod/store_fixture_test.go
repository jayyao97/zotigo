package zotigod

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/testutil/catalogtest"
)

func newTestStoreRoot(t *testing.T) string {
	t.Helper()
	return catalogtest.NewRoot(t)
}

func createTestSession(store session.Store, workDir string) (*session.Session, error) {
	sess := &session.Session{
		Metadata: session.Metadata{
			ID:               fmt.Sprintf("sess_%d", time.Now().UnixNano()),
			WorkingDirectory: workDir,
			CreatedAt:        time.Now(),
			UpdatedAt:        time.Now(),
		},
		AgentSnapshot: agent.Snapshot{
			State:     agent.StateIdle,
			CreatedAt: time.Now(),
		},
		Turns: make([]session.Turn, 0),
	}
	sess.UpdatedAt = time.Now()
	if err := store.Put(context.Background(), sess); err != nil {
		return nil, err
	}
	return sess, nil
}

func TestStoreFixtureIsolation(t *testing.T) {
	first, err := session.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	sess, err := createTestSession(first, t.TempDir())
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
	if _, err := createTestSession(second, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	items, err = first.List(context.Background(), session.ListFilter{})
	if err != nil || len(items) != 1 || items[0].ID != sess.ID {
		t.Fatalf("second store changed first store's sessions: %v, %v", items, err)
	}
}
