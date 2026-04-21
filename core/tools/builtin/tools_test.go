package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/executor"
)

func TestShellTool(t *testing.T) {
	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}
	defer exec.Close()

	tool := &ShellTool{}
	ctx := context.Background()

	t.Run("simple command", func(t *testing.T) {
		result, err := tool.Execute(ctx, exec, `{"command": "echo hello"}`)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		if !strings.Contains(result.(string), "hello") {
			t.Errorf("Expected 'hello' in output, got: %v", result)
		}
	})

	t.Run("command with workdir", func(t *testing.T) {
		// Create a subdirectory
		subDir := filepath.Join(tmpDir, "subdir")
		os.Mkdir(subDir, 0755)

		result, err := tool.Execute(ctx, exec, `{"command": "pwd", "workdir": "`+subDir+`"}`)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		if !strings.Contains(result.(string), "subdir") {
			t.Errorf("Expected 'subdir' in output, got: %v", result)
		}
	})

	t.Run("failing command", func(t *testing.T) {
		_, err := tool.Execute(ctx, exec, `{"command": "exit 1"}`)
		if err == nil {
			t.Error("Expected error for failing command")
		}
	})

	t.Run("missing command", func(t *testing.T) {
		_, err := tool.Execute(ctx, exec, `{}`)
		if err == nil {
			t.Error("Expected error for missing command")
		}
	})
}

func TestGrepTool(t *testing.T) {
	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}
	defer exec.Close()

	// Create test files
	os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("hello world\nfoo bar\nhello again"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "test.go"), []byte("package main\n\nfunc main() {\n\tprintln(\"hello\")\n}"), 0644)

	tool := &GrepTool{}
	ctx := context.Background()

	t.Run("simple search", func(t *testing.T) {
		result, err := tool.Execute(ctx, exec, `{"pattern": "hello", "path": "`+tmpDir+`"}`)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		output := result.(string)
		if !strings.Contains(output, "hello") {
			t.Errorf("Expected 'hello' in output, got: %v", output)
		}
	})

	t.Run("search with include filter", func(t *testing.T) {
		result, err := tool.Execute(ctx, exec, `{"pattern": "hello", "path": "`+tmpDir+`", "include": "*.go"}`)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		output := result.(string)
		if !strings.Contains(output, "hello") {
			t.Errorf("Expected 'hello' in output, got: %v", output)
		}
		// Should only match .go file, not .txt
		if strings.Contains(output, "test.txt") {
			t.Errorf("Should not match .txt file when filtering for .go")
		}
	})

	t.Run("no matches", func(t *testing.T) {
		result, err := tool.Execute(ctx, exec, `{"pattern": "nonexistent_pattern_xyz", "path": "`+tmpDir+`"}`)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		output := result.(string)
		if !strings.Contains(output, "No matches") {
			t.Errorf("Expected 'No matches' message, got: %v", output)
		}
	})

	t.Run("missing pattern", func(t *testing.T) {
		_, err := tool.Execute(ctx, exec, `{"path": "`+tmpDir+`"}`)
		if err == nil {
			t.Error("Expected error for missing pattern")
		}
	})
}

func TestGlobTool(t *testing.T) {
	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}
	defer exec.Close()

	// Create test file structure
	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "util.go"), []byte("package main"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# README"), 0644)
	os.Mkdir(filepath.Join(tmpDir, "src"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "src", "app.go"), []byte("package app"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "src", "app_test.go"), []byte("package app"), 0644)

	tool := &GlobTool{}
	ctx := context.Background()

	t.Run("find go files", func(t *testing.T) {
		result, err := tool.Execute(ctx, exec, `{"pattern": "*.go", "path": "`+tmpDir+`"}`)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		output := result.(string)
		if !strings.Contains(output, "main.go") {
			t.Errorf("Expected 'main.go' in output, got: %v", output)
		}
	})

	t.Run("find files in subdirectory", func(t *testing.T) {
		result, err := tool.Execute(ctx, exec, `{"pattern": "*.go", "path": "`+filepath.Join(tmpDir, "src")+`"}`)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		output := result.(string)
		if !strings.Contains(output, "app.go") {
			t.Errorf("Expected 'app.go' in output, got: %v", output)
		}
	})

	t.Run("no matches", func(t *testing.T) {
		result, err := tool.Execute(ctx, exec, `{"pattern": "*.xyz", "path": "`+tmpDir+`"}`)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		output := result.(string)
		if !strings.Contains(output, "No files found") {
			t.Errorf("Expected 'No files found' message, got: %v", output)
		}
	})

	t.Run("find directories", func(t *testing.T) {
		result, err := tool.Execute(ctx, exec, `{"pattern": "src", "path": "`+tmpDir+`", "type": "directory"}`)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		output := result.(string)
		if !strings.Contains(output, "src") {
			t.Errorf("Expected 'src' in output, got: %v", output)
		}
	})

	t.Run("missing pattern", func(t *testing.T) {
		_, err := tool.Execute(ctx, exec, `{"path": "`+tmpDir+`"}`)
		if err == nil {
			t.Error("Expected error for missing pattern")
		}
	})
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hello", "'hello'"},
		{"hello world", "'hello world'"},
		{"it's", "'it'\\''s'"},
		{"", "''"},
	}

	for _, tc := range tests {
		result := shellQuote(tc.input)
		if result != tc.expected {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.input, result, tc.expected)
		}
	}
}

func TestEditTool(t *testing.T) {
	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}
	defer exec.Close()

	tool := &EditTool{}
	ctx := context.Background()

	t.Run("simple replacement", func(t *testing.T) {
		filePath := filepath.Join(tmpDir, "test1.txt")
		os.WriteFile(filePath, []byte("hello world"), 0644)

		result, err := tool.Execute(ctx, exec, `{"path": "`+filePath+`", "old_string": "hello", "new_string": "goodbye"}`)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		if !strings.Contains(result.(string), "Successfully") {
			t.Errorf("Expected success message, got: %v", result)
		}

		// Verify content
		content, _ := os.ReadFile(filePath)
		if string(content) != "goodbye world" {
			t.Errorf("Expected 'goodbye world', got: %s", content)
		}
	})

	t.Run("delete text", func(t *testing.T) {
		filePath := filepath.Join(tmpDir, "test2.txt")
		os.WriteFile(filePath, []byte("hello world"), 0644)

		_, err := tool.Execute(ctx, exec, `{"path": "`+filePath+`", "old_string": "hello ", "new_string": ""}`)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		content, _ := os.ReadFile(filePath)
		if string(content) != "world" {
			t.Errorf("Expected 'world', got: %s", content)
		}
	})

	t.Run("replace all occurrences", func(t *testing.T) {
		filePath := filepath.Join(tmpDir, "test3.txt")
		os.WriteFile(filePath, []byte("foo bar foo baz foo"), 0644)

		result, err := tool.Execute(ctx, exec, `{"path": "`+filePath+`", "old_string": "foo", "new_string": "qux", "replace_all": true}`)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		if !strings.Contains(result.(string), "3 occurrences") {
			t.Errorf("Expected '3 occurrences' message, got: %v", result)
		}

		content, _ := os.ReadFile(filePath)
		if string(content) != "qux bar qux baz qux" {
			t.Errorf("Expected 'qux bar qux baz qux', got: %s", content)
		}
	})

	t.Run("fail on non-unique without replace_all", func(t *testing.T) {
		filePath := filepath.Join(tmpDir, "test4.txt")
		os.WriteFile(filePath, []byte("foo bar foo"), 0644)

		_, err := tool.Execute(ctx, exec, `{"path": "`+filePath+`", "old_string": "foo", "new_string": "baz"}`)
		if err == nil {
			t.Error("Expected error for non-unique old_string")
		}
		if !strings.Contains(err.Error(), "appears 2 times") {
			t.Errorf("Expected 'appears 2 times' in error, got: %v", err)
		}
	})

	t.Run("fail on not found", func(t *testing.T) {
		filePath := filepath.Join(tmpDir, "test5.txt")
		os.WriteFile(filePath, []byte("hello world"), 0644)

		_, err := tool.Execute(ctx, exec, `{"path": "`+filePath+`", "old_string": "goodbye", "new_string": "hi"}`)
		if err == nil {
			t.Error("Expected error for old_string not found")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("Expected 'not found' in error, got: %v", err)
		}
	})

	t.Run("fail on identical strings", func(t *testing.T) {
		filePath := filepath.Join(tmpDir, "test6.txt")
		os.WriteFile(filePath, []byte("hello world"), 0644)

		_, err := tool.Execute(ctx, exec, `{"path": "`+filePath+`", "old_string": "hello", "new_string": "hello"}`)
		if err == nil {
			t.Error("Expected error for identical strings")
		}
		if !strings.Contains(err.Error(), "identical") {
			t.Errorf("Expected 'identical' in error, got: %v", err)
		}
	})
}
