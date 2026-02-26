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

// GrepTool searches for patterns in files using ripgrep (rg) or fallback grep.
type GrepTool struct{}

func (t *GrepTool) Name() string { return "grep" }
func (t *GrepTool) Description() string {
	return "Search for a pattern in files. Uses ripgrep (rg) if available for better performance. Supports regex patterns."
}

func (t *GrepTool) Schema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"description": "The regex pattern to search for",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "File or directory to search in (default: current directory)",
			},
			"include": map[string]any{
				"type":        "string",
				"description": "File pattern to include (e.g., '*.go', '*.ts')",
			},
			"ignore_case": map[string]any{
				"type":        "boolean",
				"description": "Case insensitive search (default: false)",
			},
			"context_lines": map[string]any{
				"type":        "integer",
				"description": "Number of context lines to show before and after matches (default: 0)",
			},
			"max_count": map[string]any{
				"type":        "integer",
				"description": "Maximum number of matches to return (default: 100)",
			},
		},
		"required": []string{"pattern"},
	}
}

func (t *GrepTool) Safety() tools.ToolSafety {
	return tools.ToolSafety{ReadOnly: true, PathArgs: []string{"path"}}
}

func (t *GrepTool) Execute(ctx context.Context, exec executor.Executor, argsJSON string) (any, error) {
	var args struct {
		Pattern      string `json:"pattern"`
		Path         string `json:"path"`
		Include      string `json:"include"`
		IgnoreCase   bool   `json:"ignore_case"`
		ContextLines int    `json:"context_lines"`
		MaxCount     int    `json:"max_count"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if args.Pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}

	// Default values
	if args.Path == "" {
		args.Path = "."
	}
	if args.MaxCount <= 0 {
		args.MaxCount = 100
	}

	// Check if ripgrep is available
	useRg := t.hasRipgrep(ctx, exec)

	var cmd string
	if useRg {
		cmd = t.buildRgCommand(args.Pattern, args.Path, args.Include, args.IgnoreCase, args.ContextLines, args.MaxCount)
	} else {
		cmd = t.buildGrepCommand(args.Pattern, args.Path, args.Include, args.IgnoreCase, args.ContextLines, args.MaxCount)
	}

	result, err := exec.Exec(ctx, cmd, executor.ExecOptions{
		Timeout: 30 * time.Second,
	})

	// grep returns exit code 1 if no matches found
	if err != nil && result != nil && result.ExitCode == 1 && len(result.Stdout) == 0 && len(result.Stderr) == 0 {
		return "No matches found", nil
	}
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}

	output := strings.TrimSpace(string(result.Stdout))
	if output == "" {
		return "No matches found", nil
	}

	// Count matches
	lines := strings.Split(output, "\n")
	if len(lines) > args.MaxCount {
		output = strings.Join(lines[:args.MaxCount], "\n")
		output += fmt.Sprintf("\n\n... (truncated, showing %d of %d+ matches)", args.MaxCount, len(lines))
	}

	return output, nil
}

func (t *GrepTool) hasRipgrep(ctx context.Context, exec executor.Executor) bool {
	result, err := exec.Exec(ctx, "which rg", executor.ExecOptions{Timeout: 2 * time.Second})
	return err == nil && result.ExitCode == 0
}

func (t *GrepTool) buildRgCommand(pattern, path, include string, ignoreCase bool, contextLines, maxCount int) string {
	var parts []string
	parts = append(parts, "rg")
	parts = append(parts, "--line-number")
	parts = append(parts, "--no-heading")

	if ignoreCase {
		parts = append(parts, "-i")
	}
	if contextLines > 0 {
		parts = append(parts, fmt.Sprintf("-C%d", contextLines))
	}
	if include != "" {
		parts = append(parts, "-g", shellQuote(include))
	}
	if maxCount > 0 {
		parts = append(parts, fmt.Sprintf("-m%d", maxCount))
	}

	parts = append(parts, "--", shellQuote(pattern), shellQuote(path))
	return strings.Join(parts, " ")
}

func (t *GrepTool) buildGrepCommand(pattern, path, include string, ignoreCase bool, contextLines, maxCount int) string {
	var parts []string
	parts = append(parts, "grep")
	parts = append(parts, "-r")
	parts = append(parts, "-n")

	if ignoreCase {
		parts = append(parts, "-i")
	}
	if contextLines > 0 {
		parts = append(parts, fmt.Sprintf("-C%d", contextLines))
	}
	if include != "" {
		parts = append(parts, "--include", shellQuote(include))
	}
	if maxCount > 0 {
		parts = append(parts, fmt.Sprintf("-m%d", maxCount))
	}

	parts = append(parts, "--", shellQuote(pattern), shellQuote(path))
	return strings.Join(parts, " ")
}

// shellQuote wraps a string in single quotes for shell safety
func shellQuote(s string) string {
	// Replace single quotes with '\'' (end quote, escaped quote, start quote)
	escaped := strings.ReplaceAll(s, "'", "'\\''")
	return "'" + escaped + "'"
}
