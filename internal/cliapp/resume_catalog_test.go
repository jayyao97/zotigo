package cliapp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/config"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
	"github.com/jayyao97/zotigo/internal/zotigodclient"
)

func TestLoadResumeSessionsUsesServiceCatalog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(config.StateDirEnv, "")
	stateDir := filepath.Join(home, "state")
	writeGlobalTestConfig(t, home)
	workspace := createReadyWorkspace(t, stateDir)
	backend := filepath.Join(workspace.RootPath, "code", "backend")
	if err := os.MkdirAll(backend, 0o700); err != nil {
		t.Fatal(err)
	}
	plain := t.TempDir()

	ctx := context.Background()
	conn, err := zotigodclient.Connect(ctx, zotigodclient.ConnectOptions{NoDaemon: true, StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	})
	create := func(dir string) zotigodclient.Session {
		t.Helper()
		created, err := conn.CreateSession(ctx, zotigodclient.CreateSessionRequest{WorkingDirectory: dir})
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	plainSession := create(plain)
	backendSession := create(backend)

	local, err := loadResumeSessions(ctx, conn.Client, plain, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(local.sessions) != 1 || local.sessions[0].ID != plainSession.ID {
		t.Fatalf("per-directory resume = %+v", local.sessions)
	}
	if local.descriptions[plainSession.ID] != "Directory" || local.disabled[plainSession.ID] != "" {
		t.Fatalf("directory entry description=%q disabled=%q", local.descriptions[plainSession.ID], local.disabled[plainSession.ID])
	}

	all, err := loadResumeSessions(ctx, conn.Client, plain, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.sessions) != 2 {
		t.Fatalf("global resume = %+v", all.sessions)
	}
	if got := all.descriptions[backendSession.ID]; !strings.Contains(got, "Resume / Main") || !strings.Contains(got, backend) {
		t.Fatalf("workspace session description = %q", got)
	}
	if location := describeLocation(ctx, conn.Client, backendSession.ID); location != "Resume / Main" {
		t.Fatalf("location = %q", location)
	}

	if err := conn.ArchiveSession(ctx, plainSession.ID); err != nil {
		t.Fatal(err)
	}
	afterArchive, err := loadResumeSessions(ctx, conn.Client, plain, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterArchive.sessions) != 0 {
		t.Fatalf("archived session still listed: %+v", afterArchive.sessions)
	}
}

func writeGlobalTestConfig(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, config.ConfigDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const contents = "default_profile: test\nprofiles:\n  test:\n    provider: openai\n    model: test\n    api_key: fixture\n"
	if err := os.WriteFile(filepath.Join(dir, config.ConfigFileName), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func createReadyWorkspace(t *testing.T, root string) zotigoworkspace.Workspace {
	t.Helper()
	catalog, err := zotigoworkspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = catalog.Close() }()
	ctx := context.Background()
	project, err := catalog.CreateProject(ctx, "Resume")
	if err != nil {
		t.Fatal(err)
	}
	created, err := catalog.CreateWorkspace(ctx, project.ID, "Main")
	if err != nil {
		t.Fatal(err)
	}
	ready, err := catalog.ProvisionWorkspaceScaffold(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	return ready
}
