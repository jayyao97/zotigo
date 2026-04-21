package acp

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/executor"
)

// RemoteExecutor implements executor.Executor by delegating operations
// to the editor client via ACP's fs/* and terminal/* JSON-RPC methods.
type RemoteExecutor struct {
	server    *Server
	sessionID string
	workDir   string
	platform  string
}

// NewRemoteExecutor creates an executor that delegates to the ACP client.
func NewRemoteExecutor(server *Server, sessionID, workDir string) *RemoteExecutor {
	return &RemoteExecutor{
		server:    server,
		sessionID: sessionID,
		workDir:   workDir,
		platform:  runtime.GOOS,
	}
}

func (e *RemoteExecutor) ReadFile(ctx context.Context, path string) ([]byte, error) {
	content, err := e.server.ReadTextFile(ctx, e.sessionID, path)
	if err != nil {
		return nil, err
	}
	return []byte(content), nil
}

func (e *RemoteExecutor) WriteFile(ctx context.Context, path string, content []byte, _ fs.FileMode) error {
	return e.server.WriteTextFile(ctx, e.sessionID, path, string(content))
}

func (e *RemoteExecutor) Stat(ctx context.Context, path string) (*executor.FileInfo, error) {
	shell, shellArg := e.shellCmd()

	if e.platform == "windows" {
		return e.statWindows(ctx, shell, shellArg, path)
	}
	return e.statUnix(ctx, shell, shellArg, path)
}

func (e *RemoteExecutor) statUnix(ctx context.Context, shell, shellArg, path string) (*executor.FileInfo, error) {
	// Try GNU stat first, fall back to BSD stat (macOS).
	cmd := fmt.Sprintf("stat -c '%%n %%s %%F' %q 2>/dev/null || stat -f '%%N %%z %%HT' %q", path, path)
	output, exitCode, err := e.server.TerminalExec(ctx, e.sessionID, shell, []string{shellArg, cmd}, e.workDir, nil)
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

func (e *RemoteExecutor) statWindows(ctx context.Context, shell, shellArg, path string) (*executor.FileInfo, error) {
	// On Windows, check if path is a directory with "if exist <path>\* ..."
	// and check existence with "if exist <path> ...".
	cmd := fmt.Sprintf("if exist %q\\* (echo DIR) else if exist %q (echo FILE) else (echo NOTFOUND)", path, path)
	output, _, err := e.server.TerminalExec(ctx, e.sessionID, shell, []string{shellArg, cmd}, e.workDir, nil)
	if err != nil {
		return nil, err
	}

	result := strings.TrimSpace(output)
	switch result {
	case "DIR":
		return &executor.FileInfo{Name: filepath.Base(path), IsDir: true}, nil
	case "FILE":
		return &executor.FileInfo{Name: filepath.Base(path), IsDir: false}, nil
	default:
		return nil, fmt.Errorf("path not found: %s", path)
	}
}

func (e *RemoteExecutor) Exec(ctx context.Context, cmd string, opts executor.ExecOptions) (*executor.ExecResult, error) {
	cwd := opts.WorkDir
	if cwd == "" {
		cwd = e.workDir
	}

	var env []EnvVariable
	for k, v := range opts.Env {
		env = append(env, EnvVariable{Name: k, Value: v})
	}

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	shell, shellArg := e.shellCmd()

	start := time.Now()
	output, exitCode, err := e.server.TerminalExec(ctx, e.sessionID, shell, []string{shellArg, cmd}, cwd, env)
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

// shellCmd returns the shell executable and its flag for running a command string.
func (e *RemoteExecutor) shellCmd() (shell, flag string) {
	if e.platform == "windows" {
		return "cmd", "/C"
	}
	return "sh", "-c"
}

// Ensure RemoteExecutor implements executor.Executor.
var _ executor.Executor = (*RemoteExecutor)(nil)
