package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/executor"
)

func TestReadFileTool_OffsetLimit(t *testing.T) {
	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	defer exec.Close()

	path := filepath.Join(tmpDir, "sample.txt")
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	tool := &ReadFileTool{}
	ctx := context.Background()

	t.Run("no offset/limit returns full file", func(t *testing.T) {
		got, err := tool.Execute(ctx, exec, `{"path":"`+path+`"}`)
		if err != nil {
			t.Fatal(err)
		}
		if got.(string) != content {
			t.Errorf("expected full content, got %q", got)
		}
	})

	t.Run("offset skips early lines", func(t *testing.T) {
		got, err := tool.Execute(ctx, exec, `{"path":"`+path+`","offset":5}`)
		if err != nil {
			t.Fatal(err)
		}
		s := got.(string)
		if !strings.HasPrefix(s, "line 5") {
			t.Errorf("expected to start at line 5, got %q", s)
		}
		if !strings.Contains(s, "skipped 4 lines before offset") {
			t.Errorf("expected skip notice, got %q", s)
		}
	})

	t.Run("limit truncates trailing lines", func(t *testing.T) {
		got, err := tool.Execute(ctx, exec, `{"path":"`+path+`","limit":3}`)
		if err != nil {
			t.Fatal(err)
		}
		s := got.(string)
		body := strings.Split(s, "\n\n[")[0]
		if lineCount := strings.Count(body, "\n") + 1; lineCount != 3 {
			t.Errorf("expected 3 lines in body, got %d: %q", lineCount, s)
		}
		if !strings.Contains(s, "truncated 7 lines after limit") {
			t.Errorf("expected truncate notice, got %q", s)
		}
	})

	t.Run("offset+limit windows the middle", func(t *testing.T) {
		got, err := tool.Execute(ctx, exec, `{"path":"`+path+`","offset":4,"limit":2}`)
		if err != nil {
			t.Fatal(err)
		}
		body := strings.Split(got.(string), "\n\n[")[0]
		if body != "line 4\nline 5" {
			t.Errorf("expected lines 4-5, got %q", body)
		}
	})

	t.Run("offset past end returns empty marker", func(t *testing.T) {
		got, err := tool.Execute(ctx, exec, `{"path":"`+path+`","offset":999}`)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got.(string), "past end") {
			t.Errorf("expected past-end marker, got %q", got)
		}
	})
}
