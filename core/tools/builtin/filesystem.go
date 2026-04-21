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

// defaultReadLimit caps a single read_file call. Files longer than this
// return a truncated slice plus a <system-reminder> telling the model to
// follow up with offset+limit if it needs more.
const defaultReadLimit = 2000

func (t *ReadFileTool) Name() string { return "read_file" }
func (t *ReadFileTool) Description() string {
	return fmt.Sprintf("Read content of a file from the filesystem. Defaults to the first %d lines; pass offset/limit to page through larger files (1-indexed line numbers).", defaultReadLimit)
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
				"description": fmt.Sprintf("1-indexed line number to start from (optional; defaults to 1). Use together with limit to page through files larger than %d lines.", defaultReadLimit),
				"minimum":     1,
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("Maximum number of lines to return (optional; defaults to %d). Raise or combine with offset to read more.", defaultReadLimit),
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

	limit := args.Limit
	if limit <= 0 {
		limit = defaultReadLimit
	}
	return sliceLines(string(content), args.Offset, limit), nil
}

// sliceLines returns a 1-indexed [offset, offset+limit) window of lines.
// offset <= 1 means "from the start". A trailing <system-reminder> notes
// how many lines were skipped before offset or remain after the window,
// so the model can decide whether to follow up with another read_file
// call using offset+limit.
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
		return fmt.Sprintf("<system-reminder>File has %d lines; offset %d is past the end.</system-reminder>", total, offset)
	}

	end := total
	if limit > 0 && start+limit < total {
		end = start + limit
	}

	var notes []string
	if start > 0 {
		notes = append(notes, fmt.Sprintf("Skipped %d lines before offset %d.", start, offset))
	}
	if end < total {
		notes = append(notes, fmt.Sprintf("File has more lines: showing %d-%d of %d. Call read_file again with offset=%d to continue.", start+1, end, total, end+1))
	}

	body := strings.Join(lines[start:end], "\n")
	if len(notes) == 0 {
		return body
	}
	return body + "\n\n<system-reminder>" + strings.Join(notes, " ") + "</system-reminder>"
}

// --- WriteFile ---

type WriteFileTool struct{}

func (t *WriteFileTool) Name() string { return "write_file" }
func (t *WriteFileTool) Description() string {
	return "Write content to a file (overwrites existing). When the file already exists you must call read_file first so the overwrite is based on the real contents, not a guess."
}

func (t *WriteFileTool) Schema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "The absolute or relative path to the file",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "The full replacement content to write to the file",
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
	if args.Path == "" {
		return nil, fmt.Errorf("path is required")
	}

	if err := exec.WriteFile(ctx, args.Path, []byte(args.Content), 0644); err != nil {
		return nil, fmt.Errorf("failed to write file: %w", err)
	}
	return fmt.Sprintf("Successfully wrote to %s", args.Path), nil
}
