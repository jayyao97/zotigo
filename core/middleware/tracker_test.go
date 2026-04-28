package middleware_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/middleware"
	"github.com/jayyao97/zotigo/core/tools"
	"github.com/jayyao97/zotigo/core/tools/builtin"
)

// dispatch wires a tool call through a single middleware the same way
// agent.executeToolCall would, so we can test behavior end-to-end
// without spinning up the full Agent.
func dispatch(t *testing.T, mw agent.Middleware, tool tools.Tool, exec executor.Executor, args string) (any, error) {
	t.Helper()
	call := &agent.ToolCall{
		Tool:      tool,
		Name:      tool.Name(),
		Arguments: args,
		Executor:  exec,
	}
	next := func(ctx context.Context, c *agent.ToolCall) (any, error) {
		return c.Tool.Execute(ctx, c.Executor, c.Arguments)
	}
	return mw(next)(context.Background(), call)
}

func TestReadTrackerHook_MutateWithoutRead(t *testing.T) {
	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	defer exec.Close()

	path := filepath.Join(tmpDir, "f.txt")
	if err := os.WriteFile(path, []byte("hello\n"), fs.FileMode(0644)); err != nil {
		t.Fatal(err)
	}

	tracker := tools.NewReadTracker(tmpDir)
	hook := middleware.ReadTracker(tracker)

	_, err = dispatch(t, hook, &builtin.EditTool{}, exec,
		`{"path":"`+path+`","old_string":"hello","new_string":"world"}`)
	if err == nil || !strings.Contains(err.Error(), "read_file") {
		t.Fatalf("expected read_file requirement error, got %v", err)
	}
}

func TestReadTrackerHook_MutateAfterRead(t *testing.T) {
	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	defer exec.Close()

	path := filepath.Join(tmpDir, "f.txt")
	if err := os.WriteFile(path, []byte("hello\n"), fs.FileMode(0644)); err != nil {
		t.Fatal(err)
	}

	tracker := tools.NewReadTracker(tmpDir)
	hook := middleware.ReadTracker(tracker)

	if _, err := dispatch(t, hook, &builtin.ReadFileTool{}, exec, `{"path":"`+path+`"}`); err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := dispatch(t, hook, &builtin.EditTool{}, exec,
		`{"path":"`+path+`","old_string":"hello","new_string":"world"}`); err != nil {
		t.Fatalf("edit after read: %v", err)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), "world") {
		t.Errorf("expected edit applied, got %q", b)
	}
}

func TestReadTrackerHook_DetectsExternalChange(t *testing.T) {
	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	defer exec.Close()

	path := filepath.Join(tmpDir, "f.txt")
	if err := os.WriteFile(path, []byte("aaa\n"), fs.FileMode(0644)); err != nil {
		t.Fatal(err)
	}

	tracker := tools.NewReadTracker(tmpDir)
	hook := middleware.ReadTracker(tracker)

	if _, err := dispatch(t, hook, &builtin.ReadFileTool{}, exec, `{"path":"`+path+`"}`); err != nil {
		t.Fatalf("read: %v", err)
	}
	// Simulate an external edit.
	later := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(path, []byte("bbbb changed\n"), fs.FileMode(0644)); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(path, later, later)

	_, err = dispatch(t, hook, &builtin.EditTool{}, exec,
		`{"path":"`+path+`","old_string":"aaa","new_string":"zzz"}`)
	if err == nil || !strings.Contains(err.Error(), "changed on disk") {
		t.Fatalf("expected on-disk-change error, got %v", err)
	}
}

func TestReadTrackerHook_WriteFileRequiresReadForOverwrite(t *testing.T) {
	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	defer exec.Close()

	existing := filepath.Join(tmpDir, "existing.txt")
	if err := os.WriteFile(existing, []byte("old\n"), fs.FileMode(0644)); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(tmpDir, "new.txt")

	tracker := tools.NewReadTracker(tmpDir)
	hook := middleware.ReadTracker(tracker)

	// Overwriting an existing file without a prior read is refused.
	_, err = dispatch(t, hook, &builtin.WriteFileTool{}, exec,
		`{"path":"`+existing+`","content":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "read_file") {
		t.Fatalf("expected read_file requirement, got %v", err)
	}

	// Creating a brand-new file does not require a prior read.
	if _, err := dispatch(t, hook, &builtin.WriteFileTool{}, exec,
		`{"path":"`+fresh+`","content":"hello"}`); err != nil {
		t.Fatalf("create new: %v", err)
	}

	// Overwrite is OK after a read.
	if _, err := dispatch(t, hook, &builtin.ReadFileTool{}, exec, `{"path":"`+existing+`"}`); err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := dispatch(t, hook, &builtin.WriteFileTool{}, exec,
		`{"path":"`+existing+`","content":"new"}`); err != nil {
		t.Fatalf("overwrite after read: %v", err)
	}
}

func TestReadTrackerHook_NilTrackerIsNoop(t *testing.T) {
	if middleware.ReadTracker(nil) != nil {
		t.Fatal("nil tracker should yield nil middleware so agent.WithMiddleware can skip it")
	}
}
