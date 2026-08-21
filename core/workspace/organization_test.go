package workspace

import (
	"context"
	"errors"
	"testing"
)

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
