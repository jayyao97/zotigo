package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/tools"
)

// ShellTool executes shell commands through the executor.
type ShellTool struct{}

func (t *ShellTool) Name() string { return "shell" }
func (t *ShellTool) Description() string {
	return "Execute a shell command and return its output. Use this for running build commands, tests, installations, and other terminal operations."
}

func (t *ShellTool) Schema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The shell command to execute",
			},
			"workdir": map[string]any{
				"type":        "string",
				"description": "Working directory for the command (optional, defaults to current directory)",
			},
			"timeout_ms": map[string]any{
				"type":        "integer",
				"description": "Timeout in milliseconds (optional, defaults to 120000 = 2 minutes)",
			},
		},
		"required": []string{"command"},
	}
}

func (t *ShellTool) Safety() tools.ToolSafety {
	return tools.ToolSafety{ReadOnly: false}
}

func (t *ShellTool) Execute(ctx context.Context, exec executor.Executor, argsJSON string) (any, error) {
	var args struct {
		Command   string `json:"command"`
		WorkDir   string `json:"workdir"`
		TimeoutMs int    `json:"timeout_ms"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if args.Command == "" {
		return nil, fmt.Errorf("command is required")
	}

	// Default timeout: 2 minutes
	timeout := 2 * time.Minute
	if args.TimeoutMs > 0 {
		timeout = time.Duration(args.TimeoutMs) * time.Millisecond
	}

	opts := executor.ExecOptions{
		WorkDir: args.WorkDir,
		Timeout: timeout,
	}

	result, err := exec.Exec(ctx, args.Command, opts)
	if err != nil {
		return nil, fmt.Errorf("command failed: %w", err)
	}

	// Format output
	var sb strings.Builder
	if len(result.Stdout) > 0 {
		sb.Write(result.Stdout)
	}
	if len(result.Stderr) > 0 {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("[stderr]\n")
		sb.Write(result.Stderr)
	}

	output := sb.String()
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("command exited with code %d:\n%s", result.ExitCode, output)
	}

	if output == "" {
		return "Command completed successfully (no output)", nil
	}
	return output, nil
}
