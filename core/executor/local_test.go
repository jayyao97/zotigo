package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalExecutor_ReadWriteFile(t *testing.T) {
	// Create temp directory
	tmpDir := t.TempDir()

	exec, err := NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("failed to create executor: %v", err)
	}

	ctx := context.Background()

	// Test WriteFile
	content := []byte("hello world")
	err = exec.WriteFile(ctx, "test.txt", content, 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Test ReadFile
	read, err := exec.ReadFile(ctx, "test.txt")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	if string(read) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", read, content)
	}
}

func TestLocalExecutor_Exec(t *testing.T) {
	tmpDir := t.TempDir()

	exec, err := NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("failed to create executor: %v", err)
	}

	ctx := context.Background()

	// Test simple command
	result, err := exec.Exec(ctx, "echo hello", ExecOptions{})
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}

	if !result.Success() {
		t.Errorf("expected success, got exit code %d", result.ExitCode)
	}

	if string(result.Stdout) != "hello\n" {
		t.Errorf("stdout mismatch: got %q", result.Stdout)
	}
}

func TestLocalExecutor_ExecWithTimeout(t *testing.T) {
	tmpDir := t.TempDir()

	exec, err := NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("failed to create executor: %v", err)
	}

	ctx := context.Background()

	// Test command that times out
	result, err := exec.Exec(ctx, "sleep 10", ExecOptions{
		Timeout: 100 * time.Millisecond,
	})

	// Either error or non-zero exit code indicates timeout worked
	if err == nil && result.Success() {
		t.Error("expected timeout error or non-zero exit code")
	}
}

func TestLocalExecutor_ExecWithWorkDir(t *testing.T) {
	tmpDir := t.TempDir()

	// Create subdirectory
	subDir := filepath.Join(tmpDir, "subdir")
	os.MkdirAll(subDir, 0755)

	exec, err := NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("failed to create executor: %v", err)
	}

	ctx := context.Background()

	// Test pwd in subdirectory
	result, err := exec.Exec(ctx, "pwd", ExecOptions{
		WorkDir: "subdir",
	})
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}

	// Resolve symlinks for comparison (macOS /var -> /private/var)
	expected, _ := filepath.EvalSymlinks(subDir)
	got := string(result.Stdout)
	got = got[:len(got)-1] // Remove trailing newline
	gotResolved, _ := filepath.EvalSymlinks(got)

	if gotResolved != expected {
		t.Errorf("workdir mismatch: got %q, want %q", gotResolved, expected)
	}
}

func TestLocalExecutor_Stat(t *testing.T) {
	tmpDir := t.TempDir()

	exec, err := NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("failed to create executor: %v", err)
	}

	ctx := context.Background()

	// Create a file
	exec.WriteFile(ctx, "test.txt", []byte("hello"), 0644)

	// Stat file
	info, err := exec.Stat(ctx, "test.txt")
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}

	if info.Name != "test.txt" {
		t.Errorf("name mismatch: got %q", info.Name)
	}

	if info.Size != 5 {
		t.Errorf("size mismatch: got %d, want 5", info.Size)
	}

	if info.IsDir {
		t.Error("expected file, got directory")
	}
}
