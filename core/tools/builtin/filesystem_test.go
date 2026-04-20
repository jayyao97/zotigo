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

	t.Run("small file returns full content with no reminder", func(t *testing.T) {
		got, err := tool.Execute(ctx, exec, `{"path":"`+path+`"}`)
		if err != nil {
			t.Fatal(err)
		}
		s := got.(string)
		if !strings.Contains(s, "line 1") || !strings.Contains(s, "line 10") {
			t.Errorf("expected full content, got %q", s)
		}
		if strings.Contains(s, "<system-reminder>") {
			t.Errorf("small file should have no reminder, got %q", s)
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
		if !strings.Contains(s, "Skipped 4 lines before offset 5.") {
			t.Errorf("expected skip notice, got %q", s)
		}
	})

	t.Run("limit truncates trailing lines with system-reminder", func(t *testing.T) {
		got, err := tool.Execute(ctx, exec, `{"path":"`+path+`","limit":3}`)
		if err != nil {
			t.Fatal(err)
		}
		s := got.(string)
		body := strings.Split(s, "\n\n<system-reminder>")[0]
		if lineCount := strings.Count(body, "\n") + 1; lineCount != 3 {
			t.Errorf("expected 3 lines in body, got %d: %q", lineCount, s)
		}
		if !strings.Contains(s, "<system-reminder>") || !strings.Contains(s, "File has more lines") {
			t.Errorf("expected system-reminder, got %q", s)
		}
		if !strings.Contains(s, "offset=4") {
			t.Errorf("expected next-offset hint, got %q", s)
		}
	})

	t.Run("offset+limit windows the middle", func(t *testing.T) {
		got, err := tool.Execute(ctx, exec, `{"path":"`+path+`","offset":4,"limit":2}`)
		if err != nil {
			t.Fatal(err)
		}
		body := strings.Split(got.(string), "\n\n<system-reminder>")[0]
		if body != "line 4\nline 5" {
			t.Errorf("expected lines 4-5, got %q", body)
		}
	})

	t.Run("offset past end returns empty marker", func(t *testing.T) {
		got, err := tool.Execute(ctx, exec, `{"path":"`+path+`","offset":999}`)
		if err != nil {
			t.Fatal(err)
		}
		s := got.(string)
		if !strings.Contains(s, "<system-reminder>") || !strings.Contains(s, "past the end") {
			t.Errorf("expected past-end reminder, got %q", s)
		}
	})

	t.Run("default limit truncates huge files", func(t *testing.T) {
		bigPath := filepath.Join(tmpDir, "big.txt")
		var big []string
		for i := 1; i <= 2500; i++ {
			big = append(big, fmt.Sprintf("row %d", i))
		}
		if err := os.WriteFile(bigPath, []byte(strings.Join(big, "\n")+"\n"), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}

		got, err := tool.Execute(ctx, exec, `{"path":"`+bigPath+`"}`)
		if err != nil {
			t.Fatal(err)
		}
		s := got.(string)
		if !strings.Contains(s, "<system-reminder>") || !strings.Contains(s, "File has more lines") {
			t.Errorf("expected truncation reminder for big file, got tail %q", s[max(0, len(s)-300):])
		}
		if !strings.Contains(s, "offset=2001") {
			t.Errorf("expected next-offset=2001 hint, got tail %q", s[max(0, len(s)-300):])
		}
	})
}
