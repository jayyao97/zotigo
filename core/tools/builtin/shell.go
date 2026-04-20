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

// ShellTool executes shell commands through the executor. Construct
// with NewShellTool and (optionally) WithPolicy to apply a
// ShellPolicy for pattern-based blocking.
type ShellTool struct {
	policy *ShellPolicy
}

// ShellOption configures a ShellTool at construction time.
type ShellOption func(*ShellTool)

// WithPolicy attaches a ShellPolicy that will gate Classify. The policy
// is compiled on attach; compilation errors are surfaced by NewShellTool.
func WithPolicy(p *ShellPolicy) ShellOption {
	return func(t *ShellTool) { t.policy = p }
}

// NewShellTool constructs a ShellTool. Any WithPolicy() option is
// compiled before returning; a compilation error propagates to the
// caller so misconfigured policies fail fast at startup.
func NewShellTool(opts ...ShellOption) (*ShellTool, error) {
	t := &ShellTool{}
	for _, opt := range opts {
		opt(t)
	}
	if t.policy != nil {
		if err := t.policy.Compile(); err != nil {
			return nil, fmt.Errorf("shell policy: %w", err)
		}
	}
	return t, nil
}

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

func (t *ShellTool) Classify(call tools.SafetyCall) tools.SafetyDecision {
	command := tools.StringArg(call.Arguments, "command")
	if command == "" {
		return tools.SafetyDecision{Level: tools.LevelMedium, Reason: "shell command missing"}
	}

	if t.policy != nil {
		if level, reason := t.policy.Classify(command); level >= tools.LevelHigh {
			return tools.SafetyDecision{
				Level:            level,
				Reason:           reason,
				RequiresSnapshot: level < tools.LevelBlocked,
			}
		}
	}

	// Read-only whitelist bypasses approval.
	if shellCommandAppearsReadOnly(command) {
		return tools.SafetyDecision{Level: tools.LevelSafe, Reason: "read-only shell command"}
	}

	// Ambiguous mutating shell — tool can't tell what it does.
	return tools.SafetyDecision{Level: tools.LevelMedium, Reason: "shell command may mutate state", RequiresSnapshot: true}
}

// shellCommandAppearsReadOnly recognises a small set of commands we trust
// enough to execute without approval. The list is intentionally conservative.
func shellCommandAppearsReadOnly(command string) bool {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "ls", "pwd", "cat", "head", "tail", "grep", "rg", "find", "fd":
		return true
	case "git":
		return strings.HasPrefix(command, "git status") ||
			strings.HasPrefix(command, "git diff") ||
			strings.HasPrefix(command, "git log") ||
			strings.HasPrefix(command, "git show") ||
			strings.HasPrefix(command, "git branch")
	}
	return false
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
