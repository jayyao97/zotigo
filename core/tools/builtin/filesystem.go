package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/tools"
)

// --- ReadFile ---

type ReadFileTool struct{}

func (t *ReadFileTool) Name() string { return "read_file" }
func (t *ReadFileTool) Description() string {
	return "Read content of a file from the filesystem. Use offset/limit to read a slice of large files (1-indexed line numbers)."
}

func (t *ReadFileTool) Schema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "The absolute or relative path to the file",
			},
			"offset": map[string]any{
				"type":        "integer",
				"description": "1-indexed line number to start from (optional; defaults to 1). Use for large files.",
				"minimum":     1,
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Maximum number of lines to return (optional; defaults to the whole file). Use for large files.",
				"minimum":     1,
			},
		},
		"required": []string{"path"},
	}
}

func (t *ReadFileTool) Classify(call tools.SafetyCall) tools.SafetyDecision {
	if tools.IsInSafeScope(call, []string{"path"}) && !tools.IsSensitivePath(call, []string{"path"}) {
		return tools.SafetyDecision{Level: tools.LevelSafe, Reason: "file read in safe scope"}
	}
	return tools.SafetyDecision{Level: tools.LevelMedium, Reason: "file read outside safe scope or on sensitive path"}
}

func (t *ReadFileTool) Execute(ctx context.Context, exec executor.Executor, argsJSON string) (any, error) {
	var args struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	content, err := exec.ReadFile(ctx, args.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	if args.Offset <= 1 && args.Limit <= 0 {
		return string(content), nil
	}
	return sliceLines(string(content), args.Offset, args.Limit), nil
}

// sliceLines returns a 1-indexed [offset, offset+limit) window of lines.
// offset <= 1 means "from the start". limit <= 0 means "through the end".
// A trailing marker notes how many lines were skipped or truncated so the
// caller (usually the LLM) can reason about whether to request more.
func sliceLines(content string, offset, limit int) string {
	lines := strings.Split(content, "\n")
	// strings.Split("a\n", "\n") => ["a", ""]; drop the synthetic trailing
	// empty entry so line counts match the file's actual line count.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	total := len(lines)

	start := offset - 1
	if start < 0 {
		start = 0
	}
	if start >= total {
		return fmt.Sprintf("[empty: file has %d lines; offset %d is past end]", total, offset)
	}

	end := total
	if limit > 0 && start+limit < total {
		end = start + limit
	}

	var notes []string
	if start > 0 {
		notes = append(notes, fmt.Sprintf("skipped %d lines before offset", start))
	}
	if end < total {
		notes = append(notes, fmt.Sprintf("truncated %d lines after limit", total-end))
	}

	body := strings.Join(lines[start:end], "\n")
	if len(notes) == 0 {
		return body
	}
	return body + "\n\n[" + strings.Join(notes, "; ") + "]"
}

// --- WriteFile ---

type WriteFileTool struct{}

func (t *WriteFileTool) Name() string        { return "write_file" }
func (t *WriteFileTool) Description() string { return "Write content to a file (overwrites existing)" }

func (t *WriteFileTool) Schema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type": "string",
			},
			"content": map[string]any{
				"type": "string",
			},
		},
		"required": []string{"path", "content"},
	}
}

func (t *WriteFileTool) Classify(call tools.SafetyCall) tools.SafetyDecision {
	level := tools.LevelLow
	reason := "file write in working directory"
	if !tools.IsInWorkDir(call, []string{"path"}) {
		level = tools.LevelMedium
		reason = "write targets path outside working directory"
	} else if tools.IsSensitivePath(call, []string{"path"}) {
		level = tools.LevelMedium
		reason = "write targets sensitive path"
	}
	return tools.SafetyDecision{Level: level, Reason: reason, RequiresSnapshot: true}
}

func (t *WriteFileTool) Execute(ctx context.Context, exec executor.Executor, argsJSON string) (any, error) {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if err := exec.WriteFile(ctx, args.Path, []byte(args.Content), 0644); err != nil {
		return nil, fmt.Errorf("failed to write file: %w", err)
	}
	return fmt.Sprintf("Successfully wrote to %s", args.Path), nil
}

// --- ListDir ---

type ListDirTool struct{}

func (t *ListDirTool) Name() string        { return "list_dir" }
func (t *ListDirTool) Description() string { return "List files and directories" }

func (t *ListDirTool) Schema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Directory path (default .)",
			},
		},
	}
}

func (t *ListDirTool) Classify(call tools.SafetyCall) tools.SafetyDecision {
	if tools.IsInSafeScope(call, []string{"path"}) && !tools.IsSensitivePath(call, []string{"path"}) {
		return tools.SafetyDecision{Level: tools.LevelSafe, Reason: "directory listing in safe scope"}
	}
	return tools.SafetyDecision{Level: tools.LevelMedium, Reason: "directory listing outside safe scope"}
}

func (t *ListDirTool) Execute(ctx context.Context, exec executor.Executor, argsJSON string) (any, error) {
	var args struct {
		Path string `json:"path"`
	}
	json.Unmarshal([]byte(argsJSON), &args) // Ignore error, path optional

	path := args.Path
	if path == "" {
		path = "."
	}

	entries, err := exec.ListDir(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("failed to list dir: %w", err)
	}

	var result []string
	for _, e := range entries {
		prefix := "[FILE]"
		if e.IsDir {
			prefix = "[DIR] "
		}
		result = append(result, prefix+e.Name)
	}
	return result, nil
}
