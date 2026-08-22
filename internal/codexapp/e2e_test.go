package codexapp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestE2EThreadStartUsesCWDWithoutProject(t *testing.T) {
	if os.Getenv("ZOTIGO_CODEX_E2E") != "1" {
		t.Skip("set ZOTIGO_CODEX_E2E=1 to run against the installed codex binary")
	}
	binaryPath, _, err := Discover()
	if err != nil {
		t.Skipf("codex is not installed: %v", err)
	}
	t.Setenv("CODEX_HOME", t.TempDir())
	workspaceRoot := t.TempDir()
	for _, directory := range []string{"code", "notes", "artifacts"} {
		if err := os.Mkdir(filepath.Join(workspaceRoot, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runtimeDir, err := os.MkdirTemp("/tmp", "zotigo-codex-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	host, err := NewHost(binaryPath, runtimeDir, os.Stderr, HostOptions{StopWhenIdle: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, _, err := host.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Thread struct {
			ID        string  `json:"id"`
			CWD       string  `json:"cwd"`
			ProjectID *string `json:"projectId"`
		} `json:"thread"`
	}
	if err := client.Call(ctx, "thread/start", map[string]any{
		"cwd": workspaceRoot, "model": "gpt-5.6-luna", "approvalPolicy": "never",
	}, &response); err != nil {
		t.Fatal(err)
	}
	if response.Thread.ID == "" || response.Thread.CWD != workspaceRoot || response.Thread.ProjectID != nil {
		t.Fatalf("thread = %#v", response.Thread)
	}
}
