package executor

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// LocalExecutor executes operations on the local filesystem and shell
type LocalExecutor struct {
	workDir  string
	platform string
}

// NewLocalExecutor creates a new LocalExecutor with the given working directory
func NewLocalExecutor(workDir string) (*LocalExecutor, error) {
	absPath, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("invalid working directory: %w", err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("working directory does not exist: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("working directory is not a directory: %s", absPath)
	}

	return &LocalExecutor{
		workDir:  absPath,
		platform: runtime.GOOS,
	}, nil
}

// Init initializes the executor (no-op for local)
func (e *LocalExecutor) Init(ctx context.Context) error {
	return nil
}

// Close releases resources (no-op for local)
func (e *LocalExecutor) Close() error {
	return nil
}

// WorkDir returns the current working directory
func (e *LocalExecutor) WorkDir() string {
	return e.workDir
}

// Platform returns the operating system
func (e *LocalExecutor) Platform() string {
	return e.platform
}

// ReadFile reads the contents of a file
func (e *LocalExecutor) ReadFile(ctx context.Context, path string) ([]byte, error) {
	absPath := e.resolvePath(path)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	return os.ReadFile(absPath)
}

// WriteFile writes content to a file, creating parent directories if needed
func (e *LocalExecutor) WriteFile(ctx context.Context, path string, content []byte, perm fs.FileMode) error {
	absPath := e.resolvePath(path)

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Create parent directories if they don't exist
	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	if perm == 0 {
		perm = 0644
	}

	return os.WriteFile(absPath, content, perm)
}

// Stat returns file information
func (e *LocalExecutor) Stat(ctx context.Context, path string) (*FileInfo, error) {
	absPath := e.resolvePath(path)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return nil, err
	}

	return &FileInfo{
		Name:    info.Name(),
		Size:    info.Size(),
		Mode:    info.Mode(),
		ModTime: info.ModTime(),
		IsDir:   info.IsDir(),
	}, nil
}

// Exec executes a shell command
func (e *LocalExecutor) Exec(ctx context.Context, cmdStr string, opts ExecOptions) (*ExecResult, error) {
	start := time.Now()

	// Apply timeout if specified
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	// Determine shell
	shell := "/bin/sh"
	shellArg := "-c"
	if e.platform == "windows" {
		shell = "cmd"
		shellArg = "/C"
	}

	cmd := exec.CommandContext(ctx, shell, shellArg, cmdStr)

	// Set working directory
	if opts.WorkDir != "" {
		cmd.Dir = e.resolvePath(opts.WorkDir)
	} else {
		cmd.Dir = e.workDir
	}

	// Set environment
	cmd.Env = os.Environ()
	for k, v := range opts.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	// Set stdin if provided
	if opts.Stdin != nil {
		cmd.Stdin = bytes.NewReader(opts.Stdin)
	}

	// Capture output
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Run command
	err := cmd.Run()

	result := &ExecResult{
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
		Duration: time.Since(start),
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else if ctx.Err() == context.DeadlineExceeded {
			result.ExitCode = -1
			return result, fmt.Errorf("command timed out after %v", opts.Timeout)
		} else {
			result.ExitCode = -1
			return result, err
		}
	}

	return result, nil
}

// resolvePath resolves a path relative to the working directory
func (e *LocalExecutor) resolvePath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(e.workDir, path)
}

// Ensure LocalExecutor implements Executor
var _ Executor = (*LocalExecutor)(nil)
