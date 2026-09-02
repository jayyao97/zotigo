package workspace

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestMigratesV5SessionActivityTimestamp(t *testing.T) {
	root := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(root, "catalog.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE schema_meta (singleton INTEGER PRIMARY KEY CHECK(singleton = 1), version INTEGER NOT NULL CHECK(version > 0))`,
		`INSERT INTO schema_meta(singleton, version) VALUES(1, 5)`,
		`CREATE TABLE session_organization (
			session_id TEXT PRIMARY KEY, project_id TEXT, workspace_id TEXT, title TEXT,
			pinned_at INTEGER, pinned_position INTEGER, workspace_position INTEGER,
			self_archived_at INTEGER, workspace_archived_at INTEGER,
			revision INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
		)`,
		`INSERT INTO session_organization(session_id, created_at, updated_at) VALUES('session-v5', 1000, 2000)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	organization, err := store.GetSessionOrganization(context.Background(), "session-v5")
	if err != nil {
		t.Fatal(err)
	}
	if got := unixMillis(organization.ActivityAt); got != 2000 {
		t.Fatalf("activity timestamp = %d, want 2000", got)
	}
}

func TestSessionOrganizationLifecycle(t *testing.T) {
	store, workspace, _ := createGitWorkspaceFixture(t)
	ctx := context.Background()

	organization, err := store.AssignSession(ctx, "session-1", workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if organization.ProjectID == nil || *organization.ProjectID != workspace.ProjectID || organization.WorkspacePosition == nil {
		t.Fatalf("organization = %+v", organization)
	}
	if _, err := store.AssignSession(ctx, "session-1", workspace.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate assignment error = %v, want conflict", err)
	}

	organization, err = store.SetSessionTitle(ctx, "session-1", "Investigation")
	if err != nil {
		t.Fatal(err)
	}
	organization, err = store.SetSessionPinned(ctx, "session-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if organization.Title == nil || *organization.Title != "Investigation" || organization.PinnedAt == nil || organization.Revision != 3 {
		t.Fatalf("updated organization = %+v", organization)
	}
	organization, err = store.SetSessionPosition(ctx, "session-1", 500)
	if err != nil || organization.WorkspacePosition == nil || *organization.WorkspacePosition != 500 {
		t.Fatalf("positioned organization = %+v, err=%v", organization, err)
	}
	organization, err = store.SetSessionPosition(ctx, "session-1", -500)
	if err != nil || organization.WorkspacePosition == nil || *organization.WorkspacePosition != -500 {
		t.Fatalf("negative positioned organization = %+v, err=%v", organization, err)
	}

	if _, err := store.ArchiveWorkspace(ctx, workspace.ID); err != nil {
		t.Fatal(err)
	}
	organization, err = store.GetSessionOrganization(ctx, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if !organization.EffectiveArchived() || organization.WorkspaceArchivedAt == nil || organization.PinnedAt != nil {
		t.Fatalf("workspace-archived organization = %+v", organization)
	}
	if _, err := store.SetSessionPinned(ctx, "session-1", true); !errors.Is(err, ErrConflict) {
		t.Fatalf("pin archived session error = %v, want conflict", err)
	}

	if _, err := store.UnarchiveWorkspace(ctx, workspace.ID); err != nil {
		t.Fatal(err)
	}
	organization, err = store.SetSessionArchived(ctx, "session-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if !organization.EffectiveArchived() || organization.SelfArchivedAt == nil || organization.WorkspaceArchivedAt != nil {
		t.Fatalf("self-archived organization = %+v", organization)
	}
}

func TestRecordSessionActivityMovesWorkspaceSessionToFront(t *testing.T) {
	store, workspace, _ := createGitWorkspaceFixture(t)
	ctx := context.Background()
	first, err := store.AssignSession(ctx, "session-1", workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AssignSession(ctx, "session-2", workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.WorkspacePosition == nil || second.WorkspacePosition == nil || *first.WorkspacePosition >= *second.WorkspacePosition {
		t.Fatalf("initial positions = first:%v second:%v", first.WorkspacePosition, second.WorkspacePosition)
	}

	activityAt := time.Now().UTC().Add(time.Minute)
	second, err = store.RecordSessionActivity(ctx, "session-2", activityAt)
	if err != nil {
		t.Fatal(err)
	}
	if second.WorkspacePosition == nil || *second.WorkspacePosition >= *first.WorkspacePosition {
		t.Fatalf("active session position = %v, first = %v", second.WorkspacePosition, first.WorkspacePosition)
	}
	if !second.UpdatedAt.Equal(activityAt.Truncate(time.Millisecond)) {
		t.Fatalf("activity timestamp = %s, want %s", second.UpdatedAt, activityAt)
	}

	first, err = store.RecordSessionActivity(ctx, "session-1", activityAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if first.WorkspacePosition == nil || *first.WorkspacePosition >= *second.WorkspacePosition {
		t.Fatalf("next active session position = %v, previous = %v", first.WorkspacePosition, second.WorkspacePosition)
	}
}

func TestRecordSessionActivityIgnoresStaleActivity(t *testing.T) {
	store, workspace, _ := createGitWorkspaceFixture(t)
	ctx := context.Background()
	first, err := store.AssignSession(ctx, "session-1", workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AssignSession(ctx, "session-2", workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	newActivity := time.Now().UTC().Add(time.Minute)
	first, err = store.RecordSessionActivity(ctx, "session-1", newActivity)
	if err != nil {
		t.Fatal(err)
	}
	beforePosition := *first.WorkspacePosition
	beforeRevision := first.Revision
	first, err = store.RecordSessionActivity(ctx, "session-1", newActivity.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if *first.WorkspacePosition != beforePosition || first.Revision != beforeRevision {
		t.Fatalf("stale activity changed organization: %+v", first)
	}
	if second.WorkspacePosition == nil || *first.WorkspacePosition >= *second.WorkspacePosition {
		t.Fatalf("positions = first:%v second:%v", first.WorkspacePosition, second.WorkspacePosition)
	}
}

func TestRecordSessionActivityOrderDoesNotDependOnCallOrder(t *testing.T) {
	store, workspace, _ := createGitWorkspaceFixture(t)
	ctx := context.Background()
	if _, err := store.AssignSession(ctx, "session-newer", workspace.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AssignSession(ctx, "session-older", workspace.ID); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(time.Minute)
	newer, err := store.RecordSessionActivity(ctx, "session-newer", base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	older, err := store.RecordSessionActivity(ctx, "session-older", base)
	if err != nil {
		t.Fatal(err)
	}
	if newer.WorkspacePosition == nil || older.WorkspacePosition == nil || *newer.WorkspacePosition >= *older.WorkspacePosition {
		t.Fatalf("positions = newer:%v older:%v", newer.WorkspacePosition, older.WorkspacePosition)
	}
}

func TestDeleteWorkspaceRemovesOrganizationAndKeepsPathTombstone(t *testing.T) {
	store, workspace, _ := createGitWorkspaceFixture(t)
	ctx := context.Background()
	if _, err := store.AssignSession(ctx, "session-1", workspace.ID); err != nil {
		t.Fatal(err)
	}
	impact, err := store.PreviewDelete(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(impact.SessionIDs) != 1 || impact.SessionIDs[0] != "session-1" {
		t.Fatalf("delete impact sessions = %v", impact.SessionIDs)
	}
	if err := store.DeleteWorkspace(ctx, workspace.ID, workspace.Title); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetSessionOrganization(ctx, "session-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("organization after delete = %v, want not found", err)
	}
	owned, err := store.DeletedWorkspaceOwnsPath(ctx, workspace.RootPath)
	if err != nil {
		t.Fatal(err)
	}
	if !owned {
		t.Fatal("deleted workspace path tombstone is missing")
	}
}
