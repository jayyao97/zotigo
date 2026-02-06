package builtin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jayyao97/zotigo/core/executor"
)

// --- ReadFile ---

type ReadFileTool struct{}

func (t *ReadFileTool) Name() string        { return "read_file" }
func (t *ReadFileTool) Description() string { return "Read content of a file from the filesystem" }

func (t *ReadFileTool) Schema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "The absolute or relative path to the file",
			},
		},
		"required": []string{"path"},
	}
}

func (t *ReadFileTool) Execute(ctx context.Context, exec executor.Executor, argsJSON string) (any, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	content, err := exec.ReadFile(ctx, args.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}
	return string(content), nil
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
