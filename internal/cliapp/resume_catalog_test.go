package cliapp

import (
	"context"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/workspace"
)

func TestLoadGlobalResumeSessionsJoinsCatalogReadOnly(t *testing.T) {
	root := t.TempDir()
	fileStore, err := session.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fileStore.Close() })
	manager := session.NewManagerWithStore(fileStore)
	catalog, err := workspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	project, err := catalog.CreateProject(context.Background(), "Zotigo")
	if err != nil {
		t.Fatal(err)
	}
	item, err := catalog.CreateWorkspacePlan(context.Background(), project.ID, "CLI", nil)
	if err != nil {
		t.Fatal(err)
	}
	item, err = catalog.ProvisionWorkspace(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := fileStore.Put(context.Background(), &session.Session{Metadata: session.Metadata{
		ID: "sess_runtime", WorkingDirectory: item.RootPath, CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.AssignSession(context.Background(), "sess_runtime", item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.AssignSession(context.Background(), "sess_catalog_only", item.ID); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Close(); err != nil {
		t.Fatal(err)
	}

	sessions, descriptions, disabled, err := loadGlobalResumeSessions(manager)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("global sessions = %+v", sessions)
	}
	if descriptions["sess_runtime"] != "Zotigo / CLI" || disabled["sess_runtime"] != "" {
		t.Fatalf("runtime description=%q disabled=%q", descriptions["sess_runtime"], disabled["sess_runtime"])
	}
	if disabled["sess_catalog_only"] != "runtime missing" {
		t.Fatalf("catalog-only disabled reason = %q", disabled["sess_catalog_only"])
	}
}
