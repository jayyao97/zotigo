package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/tools"
)

// --- GitStatus ---

type GitStatusTool struct{}

func (t *GitStatusTool) Name() string        { return "git_status" }
func (t *GitStatusTool) Description() string { return "Show the working tree status" }
func (t *GitStatusTool) Schema() any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *GitStatusTool) Safety() tools.ToolSafety {
	return tools.ToolSafety{ReadOnly: true}
}

func (t *GitStatusTool) Execute(ctx context.Context, exec executor.Executor, argsJSON string) (any, error) {
	result, err := exec.Exec(ctx, "git status", executor.ExecOptions{})
	if err != nil {
		return nil, err
	}
	if !result.Success() {
		return nil, fmt.Errorf("git status failed: %s", result.Stderr)
	}
	return string(result.Stdout), nil
}

// --- GitDiff ---

type GitDiffTool struct{}

func (t *GitDiffTool) Name() string        { return "git_diff" }
func (t *GitDiffTool) Description() string { return "Show changes between commits, commit and working tree, etc" }
func (t *GitDiffTool) Schema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"staged": map[string]any{
				"type":        "boolean",
				"description": "Show staged changes (--cached)",
			},
		},
	}
}

func (t *GitDiffTool) Safety() tools.ToolSafety {
	return tools.ToolSafety{ReadOnly: true}
}

func (t *GitDiffTool) Execute(ctx context.Context, exec executor.Executor, argsJSON string) (any, error) {
	var args struct {
		Staged bool `json:"staged"`
	}
	json.Unmarshal([]byte(argsJSON), &args)

	cmd := "git diff"
	if args.Staged {
		cmd = "git diff --cached"
	}

	result, err := exec.Exec(ctx, cmd, executor.ExecOptions{})
	if err != nil {
		return nil, err
	}
	if !result.Success() {
		return nil, fmt.Errorf("git diff failed: %s", result.Stderr)
	}
	return string(result.Stdout), nil
}

// --- GitCommit ---

type GitCommitTool struct{}

func (t *GitCommitTool) Name() string        { return "git_commit" }
func (t *GitCommitTool) Description() string { return "Record changes to the repository" }

func (t *GitCommitTool) Schema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"message": map[string]any{
				"type": "string",
			},
		},
		"required": []string{"message"},
	}
}

func (t *GitCommitTool) Safety() tools.ToolSafety {
	return tools.ToolSafety{ReadOnly: false}
}

func (t *GitCommitTool) Execute(ctx context.Context, exec executor.Executor, argsJSON string) (any, error) {
	var args struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	// Escape quotes in message
	msg := strings.ReplaceAll(args.Message, `"`, `\"`)
	cmd := fmt.Sprintf(`git commit -m "%s"`, msg)

	result, err := exec.Exec(ctx, cmd, executor.ExecOptions{})
	if err != nil {
		return nil, err
	}
	if !result.Success() {
		return nil, fmt.Errorf("git commit failed: %s", result.Stderr)
	}
	return string(result.Stdout), nil
}

// --- GitAdd ---

type GitAddTool struct{}

func (t *GitAddTool) Name() string        { return "git_add" }
func (t *GitAddTool) Description() string { return "Add file contents to the index" }

func (t *GitAddTool) Schema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"files": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Files to add (default: all)",
			},
		},
	}
}

func (t *GitAddTool) Safety() tools.ToolSafety {
	return tools.ToolSafety{ReadOnly: false}
}

func (t *GitAddTool) Execute(ctx context.Context, exec executor.Executor, argsJSON string) (any, error) {
	var args struct {
		Files []string `json:"files"`
	}
	json.Unmarshal([]byte(argsJSON), &args)

	files := "."
	if len(args.Files) > 0 {
		files = strings.Join(args.Files, " ")
	}

	cmd := fmt.Sprintf("git add %s", files)
	result, err := exec.Exec(ctx, cmd, executor.ExecOptions{})
	if err != nil {
		return nil, err
	}
	if !result.Success() {
		return nil, fmt.Errorf("git add failed: %s", result.Stderr)
	}
	return "Files added to staging area", nil
}
