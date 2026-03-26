package acp

import (
	"context"
	"fmt"
	"io/fs"
	"runtime"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/executor"
)

// RemoteExecutor implements executor.Executor by delegating operations
// to the editor client via ACP's fs/* and terminal/* JSON-RPC methods.
type RemoteExecutor struct {
	server   *Server
	workDir  string
	platform string
}

// NewRemoteExecutor creates an executor that delegates to the ACP client.
func NewRemoteExecutor(server *Server, workDir string) *RemoteExecutor {
	return &RemoteExecutor{
		server:   server,
		workDir:  workDir,
		platform: runtime.GOOS,
	}
}

func (e *RemoteExecutor) ReadFile(ctx context.Context, path string) ([]byte, error) {
	content, err := e.server.ReadTextFile(ctx, path)
	if err != nil {
		return nil, err
	}
	return []byte(content), nil
}

func (e *RemoteExecutor) WriteFile(ctx context.Context, path string, content []byte, _ fs.FileMode) error {
	return e.server.WriteTextFile(ctx, path, string(content))
}

func (e *RemoteExecutor) ListDir(ctx context.Context, path string) ([]executor.FileInfo, error) {
	// Use terminal to run ls/dir since ACP doesn't have a native listdir
	cmd := fmt.Sprintf("ls -la %q", path)
	if e.platform == "windows" {
		cmd = fmt.Sprintf("dir %q", path)
	}

	output, exitCode, err := e.server.TerminalExec(ctx, "sh", []string{"-c", cmd}, e.workDir, nil)
	if err != nil {
		return nil, err
	}
	if exitCode != 0 {
		return nil, fmt.Errorf("ls failed (exit %d): %s", exitCode, output)
	}

	// Parse ls output into FileInfo entries (simplified)
	var entries []executor.FileInfo
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "total") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}
		name := strings.Join(fields[8:], " ")
		isDir := strings.HasPrefix(fields[0], "d")
		entries = append(entries, executor.FileInfo{
			Name:  name,
			IsDir: isDir,
		})
	}
	return entries, nil
}

func (e *RemoteExecutor) Stat(ctx context.Context, path string) (*executor.FileInfo, error) {
	// Use terminal to stat
	cmd := fmt.Sprintf("stat -c '%%n %%s %%F' %q 2>/dev/null || stat -f '%%N %%z %%HT' %q", path, path)
	output, exitCode, err := e.server.TerminalExec(ctx, "sh", []string{"-c", cmd}, e.workDir, nil)
	if err != nil {
		return nil, err
	}
	if exitCode != 0 {
		return nil, fmt.Errorf("stat failed: %s", output)
	}

	fields := strings.Fields(strings.TrimSpace(output))
	if len(fields) < 2 {
		return nil, fmt.Errorf("unexpected stat output: %s", output)
	}

	isDir := false
	for _, f := range fields[2:] {
		lower := strings.ToLower(f)
		if lower == "directory" || lower == "d" {
			isDir = true
		}
	}

	return &executor.FileInfo{
		Name:  fields[0],
		IsDir: isDir,
	}, nil
}

func (e *RemoteExecutor) MkdirAll(ctx context.Context, path string, _ fs.FileMode) error {
	cmd := fmt.Sprintf("mkdir -p %q", path)
	_, exitCode, err := e.server.TerminalExec(ctx, "sh", []string{"-c", cmd}, e.workDir, nil)
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("mkdir failed (exit %d)", exitCode)
	}
	return nil
}

func (e *RemoteExecutor) Remove(ctx context.Context, path string) error {
	cmd := fmt.Sprintf("rm -rf %q", path)
	_, exitCode, err := e.server.TerminalExec(ctx, "sh", []string{"-c", cmd}, e.workDir, nil)
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("rm failed (exit %d)", exitCode)
	}
	return nil
}

func (e *RemoteExecutor) Exec(ctx context.Context, cmd string, opts executor.ExecOptions) (*executor.ExecResult, error) {
	cwd := opts.WorkDir
	if cwd == "" {
		cwd = e.workDir
	}

	var env []string
	for k, v := range opts.Env {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	start := time.Now()
	output, exitCode, err := e.server.TerminalExec(ctx, "sh", []string{"-c", cmd}, cwd, env)
	duration := time.Since(start)
	if err != nil {
		return nil, err
	}

	return &executor.ExecResult{
		ExitCode: exitCode,
		Stdout:   []byte(output),
		Duration: duration,
	}, nil
}

func (e *RemoteExecutor) WorkDir() string  { return e.workDir }
func (e *RemoteExecutor) Platform() string { return e.platform }

func (e *RemoteExecutor) Init(_ context.Context) error { return nil }
func (e *RemoteExecutor) Close() error                 { return nil }

// Ensure RemoteExecutor implements executor.Executor.
var _ executor.Executor = (*RemoteExecutor)(nil)
