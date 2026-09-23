package workspace

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestNavigationMixedPinsAndIndependentOrdering(t *testing.T) {
	store, workspace, _ := createGitWorkspaceFixture(t)
	ctx := context.Background()
	project2, err := store.CreateProject(ctx, "Second")
	if err != nil {
		t.Fatal(err)
	}
	project3, err := store.CreateProject(ctx, "Third")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AssignSession(ctx, "session-one", workspace.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AssignSession(ctx, "session-two", workspace.ID); err != nil {
		t.Fatal(err)
	}
	projectPin := NavigationItem{Kind: "project", ID: project2.ID}
	workspacePin := NavigationItem{Kind: "workspace", ID: workspace.ID}
	sessionPin := NavigationItem{Kind: "session", ID: "session-one"}
	for _, item := range []NavigationItem{projectPin, workspacePin, sessionPin} {
		if err := store.SetNavigationPinned(ctx, item, true); err != nil {
			t.Fatal(err)
		}
	}
	originalSession, err := store.GetSessionOrganization(ctx, sessionPin.ID)
	if err != nil {
		t.Fatal(err)
	}
	desired := []NavigationItem{sessionPin, projectPin, workspacePin}
	if err := store.ReorderNavigation(ctx, "pinned", "", desired); err != nil {
		t.Fatal(err)
	}
	// Pinning twice must not move the item.
	if _, err := store.SetSessionPinned(ctx, sessionPin.ID, true); err != nil {
		t.Fatal(err)
	}
	navigation, err := store.GetNavigation(ctx)
	if err != nil || !reflect.DeepEqual(navigation.Pinned, desired) {
		t.Fatalf("navigation = %+v, %v", navigation, err)
	}
	// A second connection sees the same shared order.
	reader, err := OpenReadOnly(store.RootDir())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	other, err := reader.GetNavigation(ctx)
	if err != nil || !reflect.DeepEqual(other, navigation) {
		t.Fatalf("other client = %+v, %v", other, err)
	}
	firstProject := NavigationItem{Kind: "project", ID: workspace.ProjectID}
	thirdProject := NavigationItem{Kind: "project", ID: project3.ID}
	if err := store.ReorderNavigation(ctx, "projects", "", []NavigationItem{firstProject, projectPin, thirdProject}); err != nil {
		t.Fatal(err)
	}
	// Only visible projects are dragged; the pinned project's slot is retained.
	if err := store.ReorderNavigation(ctx, "projects", "", []NavigationItem{thirdProject, firstProject}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetNavigationPinned(ctx, projectPin, false); err != nil {
		t.Fatal(err)
	}
	navigation, err = store.GetNavigation(ctx)
	if err != nil || !reflect.DeepEqual(navigation.Projects, []NavigationItem{thirdProject, projectPin, firstProject}) {
		t.Fatalf("projects = %+v, %v", navigation, err)
	}
	if _, err := store.SetSessionPinned(ctx, sessionPin.ID, false); err != nil {
		t.Fatal(err)
	}
	session, err := store.GetSessionOrganization(ctx, sessionPin.ID)
	if err != nil || *session.WorkspacePosition != *originalSession.WorkspacePosition {
		t.Fatalf("session position changed: %+v, %v", session, err)
	}
	if _, err := store.ArchiveWorkspace(ctx, workspace.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UnarchiveWorkspace(ctx, workspace.ID); err != nil {
		t.Fatal(err)
	}
	navigation, err = store.GetNavigation(ctx)
	if err != nil || len(navigation.Pinned) != 0 {
		t.Fatalf("archived pin restored: %+v, %v", navigation, err)
	}
}

func TestNavigationReorderValidationIsAtomic(t *testing.T) {
	store, workspace, _ := createGitWorkspaceFixture(t)
	ctx := context.Background()
	pin := NavigationItem{Kind: "workspace", ID: workspace.ID}
	if err := store.SetNavigationPinned(ctx, pin, true); err != nil {
		t.Fatal(err)
	}
	before, err := store.GetNavigation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		scope, parent string
		items         []NavigationItem
		want          error
	}{
		{"pinned", "", []NavigationItem{pin, pin}, ErrInvalid},
		{"pinned", "", []NavigationItem{pin, {Kind: "workspace", ID: "missing"}}, ErrConflict},
		{"workspaces", "other-project", []NavigationItem{pin}, ErrConflict},
		{"projects", "", []NavigationItem{pin}, ErrConflict},
		{"invalid", "", []NavigationItem{pin}, ErrInvalid},
	} {
		if err := store.ReorderNavigation(ctx, test.scope, test.parent, test.items); !errors.Is(err, test.want) {
			t.Fatalf("reorder %+v: %v", test, err)
		}
	}
	after, err := store.GetNavigation(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("invalid reorder mutated navigation: %+v, %v", after, err)
	}
	if err := store.SetNavigationPinned(ctx, NavigationItem{Kind: "invalid", ID: "x"}, true); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestNavigationImportFirstClientWinsAndDropsStaleIDs(t *testing.T) {
	store, ws, _ := createGitWorkspaceFixture(t)
	ctx := context.Background()
	second, err := store.CreateProject(ctx, "Second")
	if err != nil {
		t.Fatal(err)
	}
	firstPin := NavigationItem{Kind: "project", ID: ws.ProjectID}
	secondPin := NavigationItem{Kind: "project", ID: second.ID}
	if err := store.ImportNavigation(ctx, []NavigationOrder{{Scope: "projects", Items: []NavigationItem{secondPin, {Kind: "project", ID: "deleted"}, firstPin}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ImportNavigation(ctx, []NavigationOrder{{Scope: "projects", Items: []NavigationItem{firstPin, secondPin}}}); err != nil {
		t.Fatal(err)
	}
	state, err := store.GetNavigation(ctx)
	if err != nil || !state.LegacyOrderImported || !reflect.DeepEqual(state.Projects, []NavigationItem{secondPin, firstPin}) {
		t.Fatalf("later import overwrote order: %+v, %v", state, err)
	}
	if err := store.ReorderNavigation(ctx, "projects", "", []NavigationItem{firstPin, secondPin}); err != nil {
		t.Fatal(err)
	}
	if err := store.ImportNavigation(ctx, []NavigationOrder{{Scope: "projects", Items: []NavigationItem{secondPin, firstPin}}}); err != nil {
		t.Fatal(err)
	}
	state, err = store.GetNavigation(ctx)
	if err != nil || !reflect.DeepEqual(state.Projects, []NavigationItem{firstPin, secondPin}) {
		t.Fatalf("import overwrote manual order: %+v, %v", state, err)
	}
}

func TestNavigationMigrationPreservesExistingSessionPins(t *testing.T) {
	store, workspace, _ := createGitWorkspaceFixture(t)
	ctx := context.Background()
	if _, err := store.AssignSession(ctx, "old-session", workspace.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetSessionPinned(ctx, "old-session", true); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"projects", "workspaces"} {
		for _, column := range []string{"position", "pinned_at", "pinned_position"} {
			if _, err := store.db.Exec("ALTER TABLE " + table + " DROP COLUMN " + column); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := store.db.Exec("UPDATE schema_meta SET version = 7"); err != nil {
		t.Fatal(err)
	}
	// Recreate a pre-SQL-migration catalog, including its absent journal/marker.
	if _, err := store.db.Exec("DROP TABLE catalog_migrations; DROP TABLE navigation_migration"); err != nil {
		t.Fatal(err)
	}
	if err := store.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	navigation, err := store.GetNavigation(ctx)
	if err != nil || !reflect.DeepEqual(navigation.Pinned, []NavigationItem{{Kind: "session", ID: "old-session"}}) {
		t.Fatalf("migration lost pins: %+v, %v", navigation, err)
	}
}
