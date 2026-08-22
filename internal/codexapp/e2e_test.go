package codexapp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

func TestE2EProjectCreationAndThreadAssignment(t *testing.T) {
	if os.Getenv("ZOTIGO_CODEX_E2E") != "1" {
		t.Skip("set ZOTIGO_CODEX_E2E=1 to run against the installed codex binary")
	}
	binaryPath, _, err := Discover()
	if err != nil {
		t.Skipf("codex is not installed: %v", err)
	}
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workspaceRoot := t.TempDir()
	codeRoot := filepath.Join(workspaceRoot, "code")
	if err := os.Mkdir(codeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeDir, err := os.MkdirTemp("/tmp", "zotigo-codex-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	host, err := NewHost(binaryPath, runtimeDir, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	adapter := NewAdapter(host, nil)
	if _, err := adapter.ReadWorkspace(ctx, "project-missing"); !errors.Is(err, zotigoruntime.ErrWorkspaceNotFound) {
		t.Fatalf("missing Project error = %v", err)
	}
	intent := zotigoruntime.WorkspaceCreateIntent{
		WorkspaceSpec:  zotigoruntime.WorkspaceSpec{WorkspaceID: "workspace-e2e", Name: "Workspace E2E", RootPath: codeRoot},
		IdempotencyKey: "zotigod:workspace-e2e:stable",
	}
	created, err := adapter.CreateWorkspace(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := adapter.CreateWorkspace(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != created.ID {
		t.Fatalf("idempotent create ids differ: %q != %q", replayed.ID, created.ID)
	}
	found, err := adapter.FindWorkspace(ctx, intent.WorkspaceSpec)
	if err != nil {
		t.Fatal(err)
	}
	if found == nil || found.ID != created.ID {
		t.Fatalf("find result = %#v, created=%#v", found, created)
	}

	client, _, err := host.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var threadResponse struct {
		Thread struct {
			ID        string `json:"id"`
			ProjectID string `json:"projectId"`
		} `json:"thread"`
	}
	if err := client.Call(ctx, "thread/start", map[string]any{
		"cwd": codeRoot, "model": "gpt-5.6-luna", "projectId": created.ID,
	}, &threadResponse); err != nil {
		t.Fatal(err)
	}
	if threadResponse.Thread.ID == "" || threadResponse.Thread.ProjectID != created.ID {
		t.Fatalf("thread assignment = %#v, project=%q", threadResponse.Thread, created.ID)
	}
}
