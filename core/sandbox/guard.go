package sandbox

import (
	"context"
	"fmt"
	"io/fs"

	"github.com/jayyao97/zotigo/core/executor"
)

// Compile-time check that Guard implements executor.Executor
var _ executor.Executor = (*Guard)(nil)

// CommandCheckResult contains the result of a command security check.
type CommandCheckResult struct {
	Allowed    bool
	RiskLevel  RiskLevel
	Reason     string // Why it was blocked or flagged
	Suggestion string // What the user can do
}

// PathCheckResult contains the result of a path security check.
type PathCheckResult struct {
	Allowed bool
	Reason  string
}

// Guard wraps an executor with security policy enforcement.
type Guard struct {
	executor executor.Executor
	policy   *Policy
}

// NewGuard creates a new security guard wrapping the given executor.
func NewGuard(exec executor.Executor, policy *Policy) (*Guard, error) {
	if policy == nil {
		policy = DefaultPolicy()
	}

	if err := policy.Compile(); err != nil {
		return nil, fmt.Errorf("failed to compile policy: %w", err)
	}

	return &Guard{
		executor: exec,
		policy:   policy,
	}, nil
}

// CheckCommand checks if a command is allowed to execute.
func (g *Guard) CheckCommand(cmd string) CommandCheckResult {
	level, reason := g.policy.CheckCommand(cmd)

	result := CommandCheckResult{
		RiskLevel: level,
		Reason:    reason,
	}

	switch level {
	case RiskLevelBlocked:
		result.Allowed = false
		result.Suggestion = "This command is blocked by security policy. If you need to run it, modify the sandbox policy in config."
	case RiskLevelHigh:
		result.Allowed = true // Allowed but needs extra confirmation
		result.Suggestion = "This is a high-risk operation. Please verify the command is safe before approving."
	default:
		result.Allowed = true
	}

	return result
}

// CheckPath checks if a path is allowed for file operations.
func (g *Guard) CheckPath(path string) PathCheckResult {
	allowed, reason := g.policy.CheckPath(path, g.executor.WorkDir())
	return PathCheckResult{
		Allowed: allowed,
		Reason:  reason,
	}
}

// Exec executes a command after security check.
// Returns an error if the command is blocked.
func (g *Guard) Exec(ctx context.Context, cmd string, opts executor.ExecOptions) (*executor.ExecResult, error) {
	check := g.CheckCommand(cmd)
	if !check.Allowed {
		return nil, &SecurityError{
			Operation: "exec",
			Command:   cmd,
			Reason:    check.Reason,
			RiskLevel: check.RiskLevel,
		}
	}

	return g.executor.Exec(ctx, cmd, opts)
}

// ReadFile reads a file after path check.
func (g *Guard) ReadFile(ctx context.Context, path string) ([]byte, error) {
	check := g.CheckPath(path)
	if !check.Allowed {
		return nil, &SecurityError{
			Operation: "read_file",
			Path:      path,
			Reason:    check.Reason,
		}
	}

	return g.executor.ReadFile(ctx, path)
}

// WriteFile writes a file after path check.
func (g *Guard) WriteFile(ctx context.Context, path string, content []byte, perm fs.FileMode) error {
	check := g.CheckPath(path)
	if !check.Allowed {
		return &SecurityError{
			Operation: "write_file",
			Path:      path,
			Reason:    check.Reason,
		}
	}

	return g.executor.WriteFile(ctx, path, content, perm)
}

// ListDir lists a directory after path check.
func (g *Guard) ListDir(ctx context.Context, path string) ([]executor.FileInfo, error) {
	check := g.CheckPath(path)
	if !check.Allowed {
		return nil, &SecurityError{
			Operation: "list_dir",
			Path:      path,
			Reason:    check.Reason,
		}
	}

	return g.executor.ListDir(ctx, path)
}

// Stat returns file info after path check.
func (g *Guard) Stat(ctx context.Context, path string) (*executor.FileInfo, error) {
	check := g.CheckPath(path)
	if !check.Allowed {
		return nil, &SecurityError{
			Operation: "stat",
			Path:      path,
			Reason:    check.Reason,
		}
	}

	return g.executor.Stat(ctx, path)
}

// MkdirAll creates directories after path check.
func (g *Guard) MkdirAll(ctx context.Context, path string, perm fs.FileMode) error {
	check := g.CheckPath(path)
	if !check.Allowed {
		return &SecurityError{
			Operation: "mkdir",
			Path:      path,
			Reason:    check.Reason,
		}
	}

	return g.executor.MkdirAll(ctx, path, perm)
}

// Remove removes a file or directory after path check.
func (g *Guard) Remove(ctx context.Context, path string) error {
	check := g.CheckPath(path)
	if !check.Allowed {
		return &SecurityError{
			Operation: "remove",
			Path:      path,
			Reason:    check.Reason,
		}
	}

	return g.executor.Remove(ctx, path)
}

// WorkDir returns the working directory.
func (g *Guard) WorkDir() string {
	return g.executor.WorkDir()
}

// Platform returns the platform.
func (g *Guard) Platform() string {
	return g.executor.Platform()
}

// Init initializes the executor.
func (g *Guard) Init(ctx context.Context) error {
	return g.executor.Init(ctx)
}

// Close closes the executor.
func (g *Guard) Close() error {
	return g.executor.Close()
}

// Policy returns the current policy.
func (g *Guard) Policy() *Policy {
	return g.policy
}

// Unwrap returns the underlying executor.
func (g *Guard) Unwrap() executor.Executor {
	return g.executor
}

// SecurityError represents a security policy violation.
type SecurityError struct {
	Operation string
	Command   string
	Path      string
	Reason    string
	RiskLevel RiskLevel
}

func (e *SecurityError) Error() string {
	if e.Command != "" {
		return fmt.Sprintf("security policy violation: %s blocked for command '%s': %s",
			e.Operation, truncate(e.Command, 50), e.Reason)
	}
	if e.Path != "" {
		return fmt.Sprintf("security policy violation: %s blocked for path '%s': %s",
			e.Operation, e.Path, e.Reason)
	}
	return fmt.Sprintf("security policy violation: %s blocked: %s", e.Operation, e.Reason)
}

// IsSecurityError checks if an error is a security policy violation.
func IsSecurityError(err error) bool {
	_, ok := err.(*SecurityError)
	return ok
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
