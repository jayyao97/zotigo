package builtin_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/tools"
	"github.com/jayyao97/zotigo/core/tools/builtin"
)

// trackingExec is a minimal executor wrapper that implements tools.ReadTracker
// for testing the Edit/Write Read-first enforcement.
type trackingExec struct {
	executor.Executor
	mu  sync.Mutex
	set map[string]bool
}

func newTrackingExec(base executor.Executor) *trackingExec {
	return &trackingExec{Executor: base, set: map[string]bool{}}
}

func (e *trackingExec) ReadFile(ctx context.Context, path string) ([]byte, error) {
	b, err := e.Executor.ReadFile(ctx, path)
	if err == nil {
		e.MarkRead(path)
	}
	return b, err
}

func (e *trackingExec) HasRead(path string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.set[e.key(path)]
}

func (e *trackingExec) MarkRead(path string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.set[e.key(path)] = true
}

func (e *trackingExec) key(path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(e.WorkDir(), path)
	}
	return filepath.Clean(path)
}

var _ tools.ReadTracker = (*trackingExec)(nil)

func TestEditTool_RequiresReadFirst(t *testing.T) {
	tmpDir := t.TempDir()
	base, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	defer base.Close()

	path := filepath.Join(tmpDir, "f.txt")
	if err := os.WriteFile(path, []byte("hello\n"), fs.FileMode(0644)); err != nil {
		t.Fatal(err)
	}

	exec := newTrackingExec(base)
	tool := &builtin.EditTool{}
	ctx := context.Background()

	t.Run("edit without prior read fails", func(t *testing.T) {
		_, err := tool.Execute(ctx, exec, `{"path":"`+path+`","old_string":"hello","new_string":"world"}`)
		if err == nil || !strings.Contains(err.Error(), "read_file") {
			t.Fatalf("expected read_file requirement error, got %v", err)
		}
	})

	t.Run("edit after read succeeds", func(t *testing.T) {
		if _, err := (&builtin.ReadFileTool{}).Execute(ctx, exec, `{"path":"`+path+`"}`); err != nil {
			t.Fatalf("read: %v", err)
		}
		if _, err := tool.Execute(ctx, exec, `{"path":"`+path+`","old_string":"hello","new_string":"world"}`); err != nil {
			t.Fatalf("edit after read: %v", err)
		}
		b, _ := os.ReadFile(path)
		if !strings.HasPrefix(string(b), "world") {
			t.Errorf("expected edit applied, got %q", b)
		}
	})
}

func TestWriteFileTool_RequiresReadForOverwrite(t *testing.T) {
	tmpDir := t.TempDir()
	base, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	defer base.Close()

	existing := filepath.Join(tmpDir, "existing.txt")
	if err := os.WriteFile(existing, []byte("old\n"), fs.FileMode(0644)); err != nil {
		t.Fatal(err)
	}
	brandNew := filepath.Join(tmpDir, "new.txt")

	exec := newTrackingExec(base)
	tool := &builtin.WriteFileTool{}
	ctx := context.Background()

	t.Run("overwrite without read fails", func(t *testing.T) {
		_, err := tool.Execute(ctx, exec, `{"path":"`+existing+`","content":"x"}`)
		if err == nil || !strings.Contains(err.Error(), "read_file") {
			t.Fatalf("expected read_file requirement, got %v", err)
		}
	})

	t.Run("create new file without read succeeds", func(t *testing.T) {
		if _, err := tool.Execute(ctx, exec, `{"path":"`+brandNew+`","content":"hello"}`); err != nil {
			t.Fatalf("create new: %v", err)
		}
		b, _ := os.ReadFile(brandNew)
		if string(b) != "hello" {
			t.Errorf("expected hello, got %q", b)
		}
	})

	t.Run("overwrite after read succeeds", func(t *testing.T) {
		if _, err := (&builtin.ReadFileTool{}).Execute(ctx, exec, `{"path":"`+existing+`"}`); err != nil {
			t.Fatalf("read: %v", err)
		}
		if _, err := tool.Execute(ctx, exec, `{"path":"`+existing+`","content":"new"}`); err != nil {
			t.Fatalf("overwrite after read: %v", err)
		}
	})
}
