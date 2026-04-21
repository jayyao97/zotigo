// Package executor provides an abstraction layer for executing operations
// in different environments (local, E2B sandbox, Docker, etc.)
package executor

import (
	"context"
	"io/fs"
	"time"
)

// Executor defines the interface for executing file operations and commands
// in various environments. This abstraction allows the same tool code to run
// locally, in cloud sandboxes (E2B), or in containers (Docker).
type Executor interface {
	// File operations
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, content []byte, perm fs.FileMode) error
	Stat(ctx context.Context, path string) (*FileInfo, error)

	// Command execution. Directory creation, file removal, directory
	// listing, and similar filesystem side effects all go through the
	// shell (the agent already exposes a ShellTool with a read-only
	// whitelist) — keeping the executor interface narrow mirrors the
	// Claude Code tool surface (Read / Write / Edit / Glob / Grep + Bash).
	Exec(ctx context.Context, cmd string, opts ExecOptions) (*ExecResult, error)

	// Environment info
	WorkDir() string
	Platform() string // "darwin", "linux", "windows"

	// Lifecycle
	Init(ctx context.Context) error
	Close() error
}

// FileInfo represents file metadata
type FileInfo struct {
	Name    string
	Size    int64
	Mode    fs.FileMode
	ModTime time.Time
	IsDir   bool
}

// ExecOptions configures command execution
type ExecOptions struct {
	// WorkDir overrides the default working directory
	WorkDir string
	// Env sets additional environment variables
	Env map[string]string
	// Timeout for command execution (0 = no timeout)
	Timeout time.Duration
	// Stdin provides input to the command
	Stdin []byte
}

// ExecResult contains the output of a command execution
type ExecResult struct {
	// ExitCode is the command's exit code (0 = success)
	ExitCode int
	// Stdout contains standard output
	Stdout []byte
	// Stderr contains standard error
	Stderr []byte
	// Duration is how long the command took
	Duration time.Duration
}

// Success returns true if the command exited with code 0
func (r *ExecResult) Success() bool {
	return r.ExitCode == 0
}

// CombinedOutput returns stdout and stderr combined
func (r *ExecResult) CombinedOutput() []byte {
	result := make([]byte, 0, len(r.Stdout)+len(r.Stderr))
	result = append(result, r.Stdout...)
	result = append(result, r.Stderr...)
	return result
}
