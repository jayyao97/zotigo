package workspace

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyReregisteredLocalSourceOverridesRetiredMapping(t *testing.T) {
	ctx := context.Background()
	store, existing, source := createGitWorkspaceFixture(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitProvisionCommand(t, source.CanonicalPath, "clone", "--bare", source.CanonicalPath, remote)
	runGitProvisionCommand(t, source.CanonicalPath, "remote", "add", "origin", remote)
	if err := store.migrateRepositorySources(ctx); err != nil {
		t.Fatal(err)
	}
	sources, err := store.ListSources(ctx, existing.ProjectID)
	if err != nil || len(sources) != 1 || sources[0].ID == source.ID {
		t.Fatalf("migration: %+v %v", sources, err)
	}
	replacement := sources[0]
	// dev.31 unregisters the clone and re-registers the original local path;
	// it has no knowledge of source_repositories.
	if _, err := store.db.Exec(`UPDATE sources SET registered=0 WHERE id=?`, replacement.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE sources SET registered=1 WHERE id=?`, source.ID); err != nil {
		t.Fatal(err)
	}
	local, err := store.GetSource(ctx, existing.ProjectID, source.ID)
	if err != nil || local.RepositoryID != "" {
		t.Fatalf("re-enabled local source treated as cache: %+v %v", local, err)
	}
	if _, err := store.ListSourceBranches(ctx, existing.ProjectID, source.ID); err != nil {
		t.Fatal(err)
	}
	plan, err := store.CreateWorkspacePlan(ctx, existing.ProjectID, "Old selection", []WorkspaceSourceInput{{SourceID: source.ID}})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := store.ListWorkspaceSources(ctx, plan.ID)
	if err != nil || len(bindings) != 1 || bindings[0].Source.ID != source.ID {
		t.Fatalf("resolved retired replacement: %+v %v", bindings, err)
	}
	if err := store.DeleteSource(ctx, existing.ProjectID, source.ID); err != nil {
		t.Fatal(err)
	}
	var registered bool
	if err := store.db.QueryRow(`SELECT registered FROM sources WHERE id=?`, source.ID).Scan(&registered); err != nil || registered {
		t.Fatalf("removed wrong source: %t %v", registered, err)
	}
}

func TestCompletionQueueStatesStayInvisibleToLegacyDispatcher(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.QueueInput(ctx, "s", "completion:test", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	assertHidden := func() {
		t.Helper()
		var state string
		var count int
		if err := store.db.QueryRow(`SELECT state FROM session_input_queue WHERE id='completion:test'`).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(state, "completion_") {
			t.Fatalf("lost callback authority isolation: %s", state)
		}
		if err := store.db.QueryRow(`SELECT count(*) FROM session_input_queue WHERE state IN ('queued','paused','sending')`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("legacy runnable rows: %d %v", count, err)
		}
	}
	assertHidden()
	if err := store.PauseQueuedInputs(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	assertHidden()
	rows, err := store.ClaimQueuedInputs(ctx, "s", "batch", true)
	if err != nil || len(rows) != 1 || rows[0].State != "sending" {
		t.Fatalf("claim: %+v %v", rows, err)
	}
	assertHidden()
	if err := store.FinishQueuedBatch(ctx, "s", rows[0].BatchID, false); err != nil {
		t.Fatal(err)
	}
	assertHidden()
	if err := store.ReleaseQueuedBatch(ctx, "s", rows[0].BatchID); err != nil {
		t.Fatal(err)
	}
	assertHidden()
	if err := store.RecoverQueuedInputs(ctx); err != nil {
		t.Fatal(err)
	}
	assertHidden()
	item, err := store.QueuedInputByID(ctx, "s", "completion:test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimQueuedInput(ctx, "s", item.ID, item.Revision, item.ID); err != nil {
		t.Fatal(err)
	}
	assertHidden()
	if err := store.FinishQueuedBatch(ctx, "s", item.ID, true); err != nil {
		t.Fatal(err)
	}
	assertHidden()
}
